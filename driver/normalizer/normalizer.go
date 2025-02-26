package normalizer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bblfsh/sdk/v3/uast"
	"github.com/bblfsh/sdk/v3/uast/nodes"
	. "github.com/bblfsh/sdk/v3/uast/transformer"
)

var Preprocess = Transformers([][]Transformer{
	{Mappings(Preprocessors...)},
}...)

var Normalize = Transformers([][]Transformer{
	{Mappings(
		// Move the Leading/TrailingTrivia outside of nodes.
		//
		// This cannot be inside Normalizers because it should precede any
		// other transformation.
		Map(
			opMoveTrivias{Var("group")},
			Check(Has{uast.KeyType: String(typeGroup)}, Var("group")),
		),
	)},
	{Mappings(Normalizers...)},
}...)

var _ Op = opArrHasKeyword{}

type opArrHasKeyword struct {
	keyword string
	opHas   Op
	opRest  Op
}

func (op opArrHasKeyword) Kinds() nodes.Kind {
	return nodes.KindArray
}

func (op opArrHasKeyword) Check(st *State, n nodes.Node) (bool, error) {
	arr, ok := n.(nodes.Array)
	if !ok && arr != nil {
		return false, nil
	}
	// find a node with a specified type and drop if from array
	// the boolean flag that we pass to a sub-op will indicate
	// if we found it or not
	for i, n := range arr {
		obj, ok := n.(nodes.Object)
		if !ok {
			continue
		}
		v, ok := obj[uast.KeyType]
		if !ok {
			continue
		}
		typ, ok := v.(nodes.String)
		if !ok || string(typ) != op.keyword {
			continue
		}
		// found the keyword
		if ok, err := op.opHas.Check(st, nodes.Bool(true)); err != nil || !ok {
			return ok, err
		}
		rest := make(nodes.Array, len(arr)-1)
		copy(rest, arr[:i])
		copy(rest[i:], arr[i+1:])
		return op.opRest.Check(st, rest)
	}
	// not found, default to false
	if ok, err := op.opHas.Check(st, nodes.Bool(false)); err != nil || !ok {
		return ok, err
	}
	return op.opRest.Check(st, n)
}

func (op opArrHasKeyword) Construct(st *State, n nodes.Node) (nodes.Node, error) {
	// first, we will need to read the flag from sub-op
	// if it's false, we will just pass and array as-is
	// if it's true, we will synthesize and append a node to it

	v, err := op.opHas.Construct(st, nil)
	if err != nil {
		return nil, err
	}
	has, ok := v.(nodes.Bool)
	if !ok {
		return nil, ErrUnexpectedType.New(nodes.Bool(false), v)
	}
	n, err = op.opRest.Construct(st, n)
	if err != nil {
		return nil, err
	} else if !has {
		// pass as-is
		return n, nil
	}
	// synthesize the node

	// TODO(dennwc): synthesize the node once we care about reverse transform
	//				 see https://github.com/bblfsh/sdk/issues/355
	return n, nil
}

var _ Op = opArrToChain{}

type opArrToChain struct {
	opMods Op
	opType Op
	// TODO(dennwc): maybe whitelist only known modifiers? seen so far:
	//  			 - RefKeyword
	//				 - OutKeyword (we should move it to Returns)
}

func (op opArrToChain) Kinds() nodes.Kind {
	return nodes.KindObject
}

func (op opArrToChain) Check(st *State, n nodes.Node) (bool, error) {
	// we assert that the passed node is an object and start
	// checking the Type field recursively
	// if there is one, we will remove it from the "Type" field
	// from current node and append it to an array
	// and we repeat it recursively on the value of the "Type" field
	var mods nodes.Array

	// TODO(dennwc): implement when we will need a reversal
	//				 see https://github.com/bblfsh/sdk/issues/355
	if ok, err := op.opType.Check(st, n); err != nil || !ok {
		return ok, err
	}
	return op.opMods.Check(st, mods)
}

func (op opArrToChain) Construct(st *State, n nodes.Node) (nodes.Node, error) {
	// load two nodes:
	// - the first one is an array of modifiers (objects)
	// - the second one is a type node
	nd, err := op.opMods.Construct(st, nil)
	if err != nil {
		return nil, err
	}
	mods, ok := nd.(nodes.Array)
	if !ok {
		return nil, ErrUnexpectedType.New(nodes.Array{}, nd)
	}
	typ, err := op.opType.Construct(st, n)
	if err != nil {
		return nil, err
	}
	// we will now use each modifier to construct a chain or a linked list of nodes
	// by adding a "Type" field to each modifier, that will point to the current node
	for _, nd := range mods {
		mod, ok := nd.(nodes.Object)
		if !ok {
			return nil, ErrUnexpectedType.New(nodes.Object{}, nd)
		}
		mod = mod.CloneObject()
		if _, ok := mod["Type"]; ok {
			return nil, errors.New("unexpected field in modifier: Type")
		}
		mod["Type"] = typ
		typ = mod
	}
	return typ, nil
}

// funcDefMap creates a common annotation structure for methods with a specified AST type.
//
// If returns flag is set, it will also convert the return value of the method, in other
// cases it will assume that there should be no ReturnType in the native AST node.
//
// Other object allows to remap custom fields from the native AST. Other fields can be
// either asserted to a specific value, or stored to a variable and restored in the
// FunctionGroup array using toGroup.
func funcDefMap(typ string, returns bool, other Obj, toGroup ...Op) Mapping {
	src := Obj{
		"Identifier": Var("name"),
		"ParameterList": Obj{
			uast.KeyType:         String("ParameterList"),
			uast.KeyPos:          Any(),
			"OpenParenToken":     Any(),
			"CloseParenToken":    Any(),
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
			"Parameters":         Var("params"),
		},
		"IsMissing":          Bool(false),
		"IsStructuredTrivia": Bool(false),
		"SemicolonToken":     Any(),
		"AttributeLists": Cases("caseAttrs",
			Arr(),
			Check(OfKind(nodes.KindArray), Var("attr")),
			Check(OfKind(nodes.KindObject), Var("attr")),
		),
		"Modifiers": Cases("caseMods",
			Arr(),
			NotEmpty(Var("modifiers")),
		),
	}
	for k, v := range other {
		src[k] = v
	}
	dstType := Obj{
		"Arguments": Var("params"),
	}
	if returns {
		src["ReturnType"] = Var("rettype")
		dstType["Returns"] = One(
			UASTType(uast.Argument{}, Obj{
				"Type": Var("rettype"),
			}),
		)
	}
	funcGroup := []Op{
		Cases("caseAttrs",
			Is(nil),
			Var("attr"),
			Arr(Var("attr")),
		),
		Cases("caseMods",
			Is(nil),
			NotEmpty(Var("modifiers")),
		),
	}
	funcGroup = append(funcGroup, toGroup...)
	funcGroup = append(funcGroup, UASTType(uast.Alias{}, Obj{
		"Name": Var("name"),
		"Node": UASTType(uast.Function{}, Obj{
			"Type": UASTType(uast.FunctionType{}, dstType),
			// If the function was defined with an arrow expression, we will generate
			// a uast:Block with a csharp:Return node containing the expression.
			"Body": Cases("isArrow",
				// case 1: arrow expression
				UASTType(uast.Block{}, Obj{
					uast.KeyPos: Var("arrow_pos"),
					"Statements": Arr(
						Obj{
							uast.KeyType: String("ReturnStatement"),
							uast.KeyPos:  Var("arrow_pos_tok"),
							"Expression": Var("arrow"),
						},
					),
				}),
				// case 2: full body
				// TODO(dennwc): this will definitely fail the reverse transform
				//               make a more specific node check when we need it
				//               see https://github.com/bblfsh/sdk/issues/355
				Var("body"),
			),
		}),
	}))
	return MapSemantic(typ, uast.FunctionGroup{}, MapObj(
		// Either Body or ExpressionBody will be set.
		CasesObj("isArrow",
			src,
			Objs{
				// case 1: arrow expression
				{
					"Body": Is(nil),
					"ExpressionBody": Obj{
						uast.KeyType: String("ArrowExpressionClause"),
						// will use this positions for Block in Body
						uast.KeyPos: Var("arrow_pos"),
						"ArrowToken": Obj{
							uast.KeyType: String("EqualsGreaterThanToken"),
							// will use this position for Return in Body
							uast.KeyPos: Var("arrow_pos_tok"),
							"IsMissing": Bool(false),
							"Text":      Any(),
							"Value":     Any(),
							"ValueText": Any(),
						},
						"IsMissing":          Bool(false),
						"IsStructuredTrivia": Bool(false),
						"Expression":         Var("arrow"),
					},
				},
				// case 2: full body
				{
					"ExpressionBody": Is(nil),
					"Body":           Var("body"),
				},
			},
		),

		Obj{
			"Nodes": Arr(funcGroup...),
		},
	))
}

// useFullSpan is a set of node types that use FullSpan for positions instead of Span
var useFullSpan = []nodes.Value{
	nodes.String("SingleLineDocumentationCommentTrivia"),
}

// Preprocessors is a block of AST preprocessing rules rules.
var Preprocessors = []Mapping{
	// Erase Whitespace, EndOfLine and SkippedToken trivias.
	Map(
		Obj{
			uast.KeyType: Check(
				In(
					nodes.String("WhitespaceTrivia"),
					nodes.String("EndOfLineTrivia"),
					nodes.String("SkippedTokensTrivia"),
				),
				Any(),
			),
			"FullSpan":    Any(),
			"Span":        Any(),
			"SpanStart":   Any(),
			"IsDirective": Bool(false),
		},
		// cannot delete directly, so set to nil
		Is(nil),
	),

	// Now all whitespace "SyntaxTrivia" nodes are nil, we need
	// to cleanup arrays that were hosting those nodes.
	//
	// Find "LeadingTrivia" and "TrailingTrivia" and drop nil
	// elements in those arrays.
	Map(
		Part("_", Obj{
			"LeadingTrivia": dropNils{Var("arr")},
		}),
		Part("_", Obj{
			"LeadingTrivia": Var("arr"),
		}),
	),
	Map(
		Part("_", Obj{
			"TrailingTrivia": dropNils{Var("arr")},
		}),
		Part("_", Obj{
			"TrailingTrivia": Var("arr"),
		}),
	),

	// Drop "IsEmpty" field from TextSpan.
	// We can detect it with "Length == 0", if necessary.
	Map(
		Part("_", Obj{
			uast.KeyType: String("TextSpan"),
			"IsEmpty":    Any(),
		}),
		Part("_", Obj{
			uast.KeyType: String("TextSpan"),
		}),
	),

	// Remove SpanStart from nodes. It duplicates positional info.
	// TODO(dennwc): add it as a custom position field?
	Map(
		Part("_", Obj{
			"SpanStart": Any(),
		}),
		Part("_", Obj{}),
	),

	// Positional info is stored in a child node in Span field.
	//
	// This is not supported by ObjectToNode helper, and we are
	// too lazy to create positional node by hand.
	//
	// Instead, we will temporary remap positional info to
	// "spanStart" and "spanEnd" fields of the root node, and
	// ObjectToNode will pick them up later to build a proper
	// positional node.
	//
	// There is also a FullSpan field that includes leading/trailing
	// whitespaces and sometimes node tokens. We ignore this second
	// position for most nodes, but there are few exceptions where
	// we use FullSpan and ignore Span.
	Map(
		Part("_", CasesObj("case", Obj{}, Objs{
			// exceptions - use FullSpan
			{
				uast.KeyType: Check(
					In(useFullSpan...),
					Var("typ"),
				),
				"FullSpan": Obj{
					uast.KeyType: String("TextSpan"),
					"Length":     Any(),
					"Start":      Var("start"),
					"End":        Var("end"),
				},
				// TODO(dennwc): add it as a custom position field?
				"Span": Any(),
			},
			// other nodes - use Span
			{
				uast.KeyType: Check(
					Not(In(useFullSpan...)),
					Var("typ"),
				),
				"Span": Obj{
					uast.KeyType: String("TextSpan"),
					"Length":     Any(),
					"Start":      Var("start"),
					"End":        Var("end"),
				},
				// TODO(dennwc): add it as a custom position field?
				"FullSpan": Any(),
			},
		})),
		Part("_", CasesObj("case", Obj{
			// remap to temporary keys and let ObjectToNode to pick them up
			"spanStart": Var("start"),
			"spanEnd":   Var("end"),
		}, Objs{
			// exceptions
			{
				uast.KeyType: Check(
					In(useFullSpan...),
					Var("typ"),
				),
			},
			// other nodes
			{
				uast.KeyType: Check(
					Not(In(useFullSpan...)),
					Var("typ"),
				),
			},
		})),
	),

	// Use temporary fields from the previous transform to create positional node.
	ObjectToNode{
		OffsetKey:    "spanStart",
		EndOffsetKey: "spanEnd",
	}.Mapping(),

	// Add an empty @token field to comment nodes. It's necessary to pass the check
	// in the comment extractor.
	Map(
		Part("_", Obj{
			uast.KeyType: String("SingleLineCommentTrivia"),
		}),
		Part("_", Obj{
			uast.KeyType:  String("SingleLineCommentTrivia"),
			uast.KeyToken: String(""),
		}),
	),
	Map(
		Part("_", Obj{
			uast.KeyType: String("SingleLineDocumentationCommentTrivia"),
		}),
		Part("_", Obj{
			uast.KeyType:  String("SingleLineDocumentationCommentTrivia"),
			uast.KeyToken: String(""),
		}),
	),
	Map(
		Part("_", Obj{
			uast.KeyType: String("MultiLineCommentTrivia"),
		}),
		Part("_", Obj{
			uast.KeyType:  String("MultiLineCommentTrivia"),
			uast.KeyToken: String(""),
		}),
	),
}

// Normalizers is the main block of normalization rules to convert native AST to semantic UAST.
var Normalizers = []Mapping{

	// remove empty identifier tokens
	Map(
		Check(
			Has{
				uast.KeyType: String("IdentifierToken"),
				// make sure it's empty, we don't want to wipe something useful
				"Text":      String(""),
				"Value":     String(""),
				"ValueText": String(""),
			},
			Any(),
		),
		Is(nil),
	),

	MapSemantic("IdentifierToken", uast.Identifier{}, MapObj(
		Obj{
			"IsMissing": Bool(false),

			// we drop this one, because C# allows to declare
			// a "for" identifier by using "@for" notation
			// and we don't need that token in Semantic mode
			"Text": Any(),
			// all other token values are the same
			"Value":     Var("name"),
			"ValueText": Var("name"),
		}, Obj{
			"Name": Var("name"),
		},
	)),

	// remove empty identifiers
	Map(
		Check(
			Has{
				uast.KeyType: String("IdentifierName"),
				"Identifier": Is(nil),
			},
			Any(),
		),
		Is(nil),
	),

	Map(
		Obj{
			uast.KeyType: String("IdentifierName"),
			uast.KeyPos:  Any(), // TODO(dennwc): assert that it's the same

			"Identifier": Var("ident"),

			"Arity": Int(0),

			// TODO(dennwc): these assertions might not be valid for all cases
			//               and will break this annotation, but at least it will
			//               help us detect the case when it's not valid
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),

			// TODO(dennwc): this is true for Value == "unmanaged" and it looks
			//				 more like a keyword, probably unrecognized one
			"IsUnmanaged": Any(),

			// TODO(dennwc): might be useful later; drop it for now
			"IsVar": Any(),
		},
		Var("ident"),
	),

	// Special: is a keyword, but used as an identifier (Parameter name)
	MapSemantic("ArgListKeyword", uast.Identifier{}, MapObj(
		Obj{
			"IsMissing": Bool(false),

			// all token values are the same
			"Text":      String("__arglist"),
			"Value":     String("__arglist"),
			"ValueText": String("__arglist"),
		}, Obj{
			"Name": String("__arglist"),
		},
	)),

	MapSemantic("StringLiteralExpression", uast.String{}, MapObj(
		Obj{
			"Token": Obj{
				uast.KeyType: String("StringLiteralToken"),
				uast.KeyPos:  Any(),

				"IsMissing": Bool(false),

				// contains escaped value, we don't need it in canonical UAST
				"Text": Any(),

				// both values are the same
				"Value":     Var("val"),
				"ValueText": Var("val"),
			},
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
		},
		Obj{
			"Value": Var("val"),
		},
	)),

	// A string literal part of the interpolation expression.
	MapSemantic("InterpolatedStringTextToken", uast.String{}, MapObj(
		Obj{
			// trivia == whitespace; can safely drop it
			"LeadingTrivia":  Arr(),
			"TrailingTrivia": Arr(),

			"IsMissing": Bool(false),

			// contains escaped value, we don't need it in canonical UAST
			"Text": Any(),

			// both values are the same
			"Value":     Var("val"),
			"ValueText": Var("val"),
		},
		Obj{
			"Value": Var("val"),
		},
	)),

	// Collapse one more AST level if the string token is inside InterpolatedStringText.
	MapSemantic("InterpolatedStringText", uast.String{}, MapObj(
		Obj{
			"TextToken": Obj{
				uast.KeyType: String(uast.TypeOf(uast.String{})),
				uast.KeyPos:  Any(),
				"Format":     String(""),
				"Value":      Var("val"),
			},
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
		},
		Obj{
			"Value": Var("val"),
		},
	)),

	MapSemantic("TrueLiteralExpression", uast.Bool{}, MapObj(
		Obj{
			"Token": Obj{
				uast.KeyType: String("TrueKeyword"),
				uast.KeyPos:  Any(),

				"Text":      String("true"),
				"Value":     Bool(true),
				"ValueText": String("true"),

				"IsMissing": Bool(false),
			},
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
		},
		Obj{
			"Value": Bool(true),
		},
	)),

	MapSemantic("FalseLiteralExpression", uast.Bool{}, MapObj(
		Obj{
			"Token": Obj{
				uast.KeyType: String("FalseKeyword"),
				uast.KeyPos:  Any(),

				"Text":      String("false"),
				"Value":     Bool(false),
				"ValueText": String("false"),

				"IsMissing": Bool(false),
			},
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
		},
		Obj{
			"Value": Bool(false),
		},
	)),

	MapSemantic("Block", uast.Block{}, MapObj(
		Obj{
			"Statements": Var("stmts"),
			// TODO(dennwc): remap to custom positional fields
			"OpenBraceToken":     Any(),
			"CloseBraceToken":    Any(),
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
		},
		Obj{
			"Statements": Var("stmts"),
		},
	)),

	MapSemantic("SingleLineCommentTrivia", uast.Comment{}, MapObj(
		Obj{
			uast.KeyToken: CommentText([2]string{"//", ""}, "text"),
			"IsDirective": Bool(false),
		},
		CommentNode(false, "text", nil),
	)),

	MapSemantic("MultiLineCommentTrivia", uast.Comment{}, MapObj(
		Obj{
			uast.KeyToken: CommentText([2]string{"/*", "*/"}, uast.KeyToken),
			"IsDirective": Bool(false),
		},
		CommentNode(true, uast.KeyToken, nil),
	)),

	// TODO(dennwc): differentiate from regular comments
	MapSemantic("SingleLineDocumentationCommentTrivia", uast.Comment{}, MapObj(
		Obj{
			uast.KeyToken: CommentText([2]string{"///", ""}, "text"),
			"IsDirective": Bool(false),
		},
		CommentNode(false, "text", nil),
	)),

	// Import (aka UsingDirectiveSyntax) is more or less trivial.
	//
	// "Name" field is QualifiedIdentifier or Identifier and we remap to
	// "Path" in Import.
	//
	// StaticKeyword indicates if the "using" directive is static.
	// In this case we Set a import target scope to an object with static = true.
	//
	// Alias field may be set as well - we will as usual remap it to
	// an uast:Alias object in the Import's Path field.
	//
	// Also, C# assumes that "using" statement imports all the symbols
	// from that package, so we also set an "All" field on Import.
	MapSemantic("UsingDirective", uast.Import{}, MapObj(
		Obj{
			"Name": Var("path"),

			// TODO(dennwc): remap to custom positional fields
			"SemicolonToken": Any(),
			"UsingKeyword":   Any(),

			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
			"StaticKeyword": If("isStatic",
				Check(HasType("StaticKeyword"), Any()),
				Check(HasType("None"), Any()),
			),
			"Alias": If("isAlias",
				Obj{
					uast.KeyType:         String("NameEquals"),
					uast.KeyPos:          Any(),
					"EqualsToken":        Any(),
					"IsMissing":          Bool(false),
					"IsStructuredTrivia": Bool(false),
					"Name":               Var("alias"),
				},
				Is(nil),
			),
		},
		Obj{
			"Path": If("isAlias",
				UASTType(uast.Alias{}, Obj{
					"Name": Var("alias"),
					"Node": Var("path"),
				}),
				Var("path"),
			),
			"All": Bool(true),
			"Target": If("isStatic",
				Obj{"static": Bool(true)},
				Is(nil),
			),
		},
	)),

	// QualifiedIdentifier case is interesting in the sense that AST nodes
	// are organized as a linked list.
	//
	// The root QualifiedNameSyntax node will have the "Right" field pointing
	// to an Identifier (it was already converted from IdentifierNameSyntax)
	// and the "Left" field may either point to another Identifier or
	// to the next QualifiedNameSyntax (down to root of the package hierarchy).
	//
	// For the first case, we create a single QualifiedIdentifier node
	// by making a "Names" array from "Left" and "Right" Identifiers.
	//
	// For the second case, we rely on the fact that transforms are
	// using the DFS order. We assert that "Left" is already a
	// QualifiedIdentifier (all children were converted by DFS) and
	// save its "Names". Then we can simply create a new QualifiedIdentifier
	// and append "Right" (Identifier) to the end of the saved "Names" array.
	MapSemantic("QualifiedName", uast.QualifiedIdentifier{}, MapObj(
		CasesObj("case",
			// common
			Obj{
				"Right":              Var("right"),
				"Arity":              Int(0),
				"DotToken":           Any(),
				"IsMissing":          Bool(false),
				"IsStructuredTrivia": Bool(false),
				"IsUnmanaged":        Bool(false),
				"IsVar":              Bool(false),
			},
			// cases
			Objs{
				// the last name = identifier
				{
					"Left": Check(HasType(uast.Identifier{}), Var("left")),
				},
				// linked list
				{
					"Left": UASTType(uast.QualifiedIdentifier{}, Obj{
						// FIXME: start position
						uast.KeyPos: Any(),
						"Names":     Var("names"),
					}),
				},
			},
		),
		CasesObj("case", nil,
			Objs{
				// the last name = identifier
				{
					"Names": Arr(Var("left"), Var("right")),
				},
				// linked list
				{
					"Names": Append(Var("names"), Arr(Var("right"))),
				},
			},
		),
	)),

	// Old style multiple arguments: argument with the magic name "__arglist"
	MapSemantic("Parameter", uast.Argument{}, MapObj(
		Obj{
			"Identifier": Check(
				Has{
					uast.KeyType: String(uast.TypeOf(uast.Identifier{})),
					"Name":       String("__arglist"),
				}, Var("name")),
			"AttributeLists":     Arr(), // TODO(dennwc): any cases when it's not empty?
			"Default":            Var("def_init"),
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
			"Modifiers":          Arr(), // TODO(dennwc): any cases when it's not empty?
			"Type":               Var("type"),
		},
		Obj{
			"Name":        Var("name"),
			"Type":        Var("type"),
			"Init":        Var("def_init"),
			"Variadic":    Bool(true),
			"MapVariadic": Bool(false),
			"Receiver":    Bool(false),
		},
	)),

	// Normal parameter, potential multiple args expressed by "params" in modifiers
	MapSemantic("Parameter", uast.Argument{}, MapObj(
		Obj{
			"Identifier": Check(Has{
				uast.KeyType: String(uast.TypeOf(uast.Identifier{})),
			}, Var("name")),
			"AttributeLists":     Arr(), // TODO(dennwc): any cases when it's not empty?
			"Default":            Var("def_init"),
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Any(),
			"Modifiers": opArrHasKeyword{
				keyword: "ParamsKeyword",
				opHas:   Var("variadic"),
				opRest: opArrHasKeyword{
					keyword: "ThisKeyword",
					opHas:   Var("this"),
					opRest:  Var("rest"),
				},
			},
			"Type": Var("type"),
		},
		Obj{
			"Name": Var("name"),
			"Type": opArrToChain{
				opMods: Var("rest"),
				opType: Var("type"),
			},
			"Init":        Var("def_init"),
			"Variadic":    Var("variadic"),
			"MapVariadic": Bool(false),
			"Receiver":    Var("this"),
		},
	)),

	funcDefMap("MethodDeclaration", true,
		Obj{
			// number of parameters - safe to ignore
			"Arity":                      Any(),
			"ExplicitInterfaceSpecifier": Is(nil),
			"ConstraintClauses": Cases("caseConstraint",
				Arr(),
				NotEmpty(Var("constraints")),
			),
			"TypeParameterList": Cases("caseTypeParams",
				Is(nil),
				Arr(),
				NotEmpty(Var("typeParams")),
			),
		},
		Cases("caseTypeParams",
			Is(nil),
			Is(nil),
			NotEmpty(Var("typeParams")),
		),
		Cases("caseConstraint",
			Is(nil),
			NotEmpty(Var("constraints")),
		),
	),
	// ConstructorDeclaration is similar to MethodDeclaration, but it may include a
	// base class initializer that require a special transformation.
	MapSemantic("ConstructorDeclaration", uast.FunctionGroup{}, MapObj(
		Obj{
			// Same as to MethodDeclaration above
			"Identifier": Var("name"),
			"ParameterList": Obj{
				uast.KeyType:         String("ParameterList"),
				uast.KeyPos:          Any(),
				"OpenParenToken":     Any(),
				"CloseParenToken":    Any(),
				"IsMissing":          Bool(false),
				"IsStructuredTrivia": Bool(false),
				"Parameters":         Var("params"),
			},
			"IsMissing":          Bool(false),
			"IsStructuredTrivia": Bool(false),
			"SemicolonToken":     Any(),
			"AttributeLists": Cases("caseAttrs",
				Arr(),
				Check(OfKind(nodes.KindArray), Var("attr")),
				Check(OfKind(nodes.KindObject), Var("attr")),
			),

			// won't be set for a constructor
			"ExpressionBody": Is(nil),

			// Initializer is an expression to initialize the base class.
			// Here we consider case if it's exists in the AST node.
			//
			// Initializer is basically a function call that will init the base class.
			// So we will consider it a first statement of a function body.
			//
			// For this we dig into Body above to get the list of statements.
			// If hasBaseInit is set (initializer exists), we will prepend it
			// to the array of statement in Body.
			"Initializer": If("hasBaseInit",
				// case 1: has base initializer
				Check(HasType("BaseConstructorInitializer"), Var("baseInit")),
				// case 2: no initializer
				Is(nil),
			),
			"Body": Part("bodyMap", Obj{
				"Statements": Var("stmts"),
			}),
			"Modifiers": Cases("caseMods",
				Arr(),
				NotEmpty(Var("modifiers")),
			),
		},
		Obj{
			"Nodes": Arr(
				Cases("caseAttrs",
					Is(nil),
					Var("attr"),
					Arr(Var("attr")),
				),
				Cases("caseMods",
					Is(nil),
					NotEmpty(Var("modifiers")),
				),
				UASTType(uast.Alias{}, Obj{
					"Name": Var("name"),
					"Node": UASTType(uast.Function{}, Obj{
						// Restore the function body.
						//
						// We will also prepend a base class initializer here, see above.
						"Body": Part("bodyMap", Obj{
							"Statements": If("hasBaseInit",
								// case 1: has base initializer
								PrependOne(Var("baseInit"), Var("stmts")),
								// case 2: no initializer
								Var("stmts"),
							),
						}),
						"Type": UASTType(uast.FunctionType{}, Obj{
							"Arguments": Var("params"),
						}),
					}),
				}),
			),
		},
	)),
	funcDefMap("DestructorDeclaration", false, Obj{
		"TildeToken": Any(),
	}),

	// Merge uast:Group with uast:FunctionGroup.
	Map(
		opMergeGroups{Var("group")},
		Check(
			Has{uast.KeyType: In(
				nodes.String(typeFuncGroup),
				nodes.String(typeGroup),
			)},
			Var("group"),
		),
	),
}

// dropNils accepts a array node, removes all nil values from it and passes it to
// a specified suboperation.
// It will not restore nil values when constructing nodes (not reversible).
type dropNils struct {
	op Op
}

func (op dropNils) Kinds() nodes.Kind {
	return nodes.KindArray
}

func (op dropNils) Check(st *State, n nodes.Node) (bool, error) {
	arr, ok := n.(nodes.Array)
	if !ok && n != nil {
		return false, nil
	}
	out := make(nodes.Array, 0, len(arr))
	for _, e := range arr {
		if e != nil {
			out = append(out, e)
		}
	}
	return op.op.Check(st, out)
}

func (op dropNils) Construct(st *State, n nodes.Node) (nodes.Node, error) {
	return op.op.Construct(st, n)
}

var (
	typeGroup     = uast.TypeOf(uast.Group{})
	typeFuncGroup = uast.TypeOf(uast.FunctionGroup{})
)

// triviaField specified a field with an array to put trivias into.
var triviaField = map[string]string{
	"Block":           "Statements",
	"CompilationUnit": "Members",
}

// firstWithType returns an index of the first node type of which matches the filter function.
func firstWithType(arr nodes.Array, fnc func(typ string) bool) int {
	for i, sub := range arr {
		if fnc(uast.TypeOf(sub)) {
			return i
		}
	}
	return -1
}

// opMoveTrivias cuts trivia nodes from LeadingTrivia/TrailingTrivia fields
// and wraps the node into uast:Group that contains those trivias.
type opMoveTrivias struct {
	sub Op
}

func (op opMoveTrivias) Kinds() nodes.Kind {
	return nodes.KindObject
}

// Check implements the logic to move the trivia out of the node into a Group that will
// wrap the trivia and the node itself.
//
// The function is a bit more complex than it should be because there are some edge cases
// where instead of wrapping the node we will add trivia to a specific field of the node.
//
// There are two general cases here that may overlap.
//
// First, some nodes will have a Leading/TrailingTrivia fields. We will remove those fields
// from the node, and will wrap the node itself into a uast:Group with an array consisting
// of (leading trivia + node + trailing trivia).
//
// There are some edge cases here. For example, we don't want to wrap a CompilationUnit nodes
// so we will add trivia nodes to it's Members slice (see triviaField map for all such cases).
//
// But, we can't stop here because the Group will prevent other annotations to match against
// the original node. To fix this, we also try to access few known fields and check if they
// already contain a Group. If it does, we unwrap the node and join its trivia into our
// leading/trailing slice. This is possible because DSL guarantees that transforms always
// happen in DFS order, thus all child nodes were already visited by this transform.
func (op opMoveTrivias) Check(st *State, n nodes.Node) (bool, error) {
	obj, ok := n.(nodes.Object)
	if !ok {
		return false, nil
	}
	modified := false
	leading, ok1 := obj["LeadingTrivia"].(nodes.Array)
	trailing, ok2 := obj["TrailingTrivia"].(nodes.Array)
	if ok1 || ok2 {
		// we saved the leading and trailing trivias, now wipe them from the node
		obj = obj.CloneObject()
		modified = true
		delete(obj, "LeadingTrivia")
		delete(obj, "TrailingTrivia")
	}

	// move comments out of keywords and tokens into actual AST nodes
	// TODO(dennwc): handle more cases:
	//				 - Modifiers?
	for key, sub := range obj {
		if key != "ReturnType" &&
			!strings.HasSuffix(key, "Token") &&
			!strings.HasSuffix(key, "Keyword") {
			continue
		}
		if uast.TypeOf(sub) != typeGroup {
			continue
		}
		group := sub.(nodes.Object)
		arr, ok := group["Nodes"].(nodes.Array)
		if !ok {
			continue
		}
		// we already joined trivias and the node into a single array,
		// so we will have to find its index now
		ind := firstWithType(arr, func(typ string) bool {
			return !strings.HasSuffix(typ, "Trivia")
		})

		var node nodes.Node
		if ind < 0 {
			// cannot determine an index - pretend everything is a leading trivia
			// TODO(dennwc): this should not happen in practice, since leading/trailing
			//  			 trivias are always attached to a node, and this node should
			//  			 be somewhere in this slice
			//  			 but future annotations may drop this node, so we will need
			//  			 to find a different way of splitting leading and trailing
			// 				 trivia if it happens in practice
			leading = append(leading.CloneList(), arr...)
		} else {
			node = arr[ind]
			leading = append(leading.CloneList(), arr[:ind]...)
			trailing = append(arr[ind+1:len(arr):len(arr)], trailing...)
		}
		if !modified {
			obj = obj.CloneObject()
			modified = true
		}
		obj[key] = node
	}

	if len(leading) == 0 && len(trailing) == 0 {
		if !modified {
			return false, nil // unmodified
		}
		// removed the trivia fields
		return op.sub.Check(st, obj)
	}

	if field, ok := triviaField[uast.TypeOf(obj)]; ok {
		// instead of wrapping the node, join trivias to a specified field
		old, ok := obj[field].(nodes.Array)
		if !ok {
			return false, fmt.Errorf("expected %q field to be an array", field)
		}
		arr := make(nodes.Array, 0, len(leading)+len(old)+len(trailing))
		arr = append(arr, leading...)
		arr = append(arr, old...)
		arr = append(arr, trailing...)

		obj[field] = arr
		return op.sub.Check(st, obj)
	}

	// wrap the node into a uast:Group
	arr := make(nodes.Array, 0, len(leading)+1+len(trailing))
	arr = append(arr, leading...)
	arr = append(arr, obj)
	arr = append(arr, trailing...)

	// TODO(dennwc): it will be nice if we could extract FullSpan position into the Group
	group, err := uast.ToNode(uast.Group{})
	if err != nil {
		return false, err
	}

	// note that we overwrite a variable - it was the current node
	// and now it is a Group wrapping the current node
	obj = group.(nodes.Object)
	obj["Nodes"] = arr
	return op.sub.Check(st, obj)
}

func (op opMoveTrivias) Construct(st *State, n nodes.Node) (nodes.Node, error) {
	// TODO(dennwc): implement when we will need a reversal
	//				 see https://github.com/bblfsh/sdk/issues/355
	return op.sub.Construct(st, n)
}

// opMergeGroups finds the uast:Group nodes and merges them into a child
// uast:FunctionGroup, if it exists.
//
// This transform is necessary because opMoveTrivias will wrap all nodes that contain trivia
// into a Group node, and the same will happen with MethodDeclaration nodes. But according
// to a UAST schema defined in SDK, the comments (trivia) should be directly inside the
// FunctionGroup node that wraps functions in Semantic mode.
type opMergeGroups struct {
	sub Op
}

func (op opMergeGroups) Kinds() nodes.Kind {
	return nodes.KindObject
}

// Check tests if a current node is uast:Group and uast:FuncGroup and contains group of
// another kind. It will remove the second group and merge children into current one.
// uast:FuncGroup is preferred.
//
// See https://github.com/bblfsh/sdk/issues/361.
func (op opMergeGroups) Check(st *State, n nodes.Node) (bool, error) {
	group, ok := n.(nodes.Object)
	if !ok {
		return false, nil
	}
	switch uast.TypeOf(group) {
	case typeGroup:
		return op.checkGroup(st, group)
	case typeFuncGroup:
		return op.checkFuncGroup(st, group)
	}
	return false, nil
}

// checkGroup tests if the current node is uast:Group and if it contains a uast:FunctionGroup
// node, it will rebuild the node by removing the current node and merge other children into
// the FunctionGroup. The changed node will be passed to the sub-operation.
func (op opMergeGroups) checkGroup(st *State, group nodes.Object) (bool, error) {
	arr, ok := group["Nodes"].(nodes.Array)
	if !ok {
		return false, errors.New("expected an array in Group.Nodes")
	}
	ind := firstWithType(arr, func(typ string) bool {
		return typ == typeFuncGroup
	})
	if ind < 0 {
		return false, nil
	}
	leading := arr[:ind]
	fgroup := arr[ind].(nodes.Object)
	trailing := arr[ind+1:]

	arr, ok = fgroup["Nodes"].(nodes.Array)
	if !ok {
		return false, errors.New("expected an array in Group.Nodes")
	}
	out := make(nodes.Array, 0, len(leading)+len(arr)+len(trailing))
	out = append(out, leading...)
	out = append(out, arr...)
	out = append(out, trailing...)

	fgroup = fgroup.CloneObject()
	fgroup["Nodes"] = out

	return op.sub.Check(st, fgroup)
}

// checkFuncGroup tests if the current node is uast:FuncGroup and if any of its sub-arrays
// contain a uast:Group, it will be removed and the children will be flattened into a sub-array.
func (op opMergeGroups) checkFuncGroup(st *State, fgroup nodes.Object) (bool, error) {
	// primary nodes array in the function group
	arr, ok := fgroup["Nodes"].(nodes.Array)
	if !ok {
		return false, errors.New("expected an array in FuncGroup.Nodes")
	}
	modified := false
	for i := 0; i < len(arr); i++ {
		v := arr[i]
		// Since we will run this for every group, let's clean a group
		// array from nils here. It could be a separate transform, but
		// it may affect performance, effectively running all checks twice.
		if v == nil {
			if !modified {
				arr = arr.CloneList()
				modified = true
			}
			arr = append(arr[:i], arr[i+1:]...)
			i--
			continue
		}
		// secondary arrays the group annotations/modifiers, etc
		arr2, ok := v.(nodes.Array)
		if !ok {
			continue
		}
		// find a group node there
		ind := firstWithType(arr2, func(typ string) bool {
			return typ == typeGroup
		})
		if ind < 0 {
			continue
		}
		group := arr2[ind].(nodes.Object)
		// children array of an inner group
		arr3, ok := group["Nodes"].(nodes.Array)
		if !ok {
			return false, errors.New("expected an array in Group.Nodes")
		}
		// flatten inner array into the secondary array
		out := make(nodes.Array, 0, len(arr2)-1+len(arr3))
		out = append(out, arr2[:ind]...)
		out = append(out, arr3...)
		out = append(out, arr2[ind+1:]...)
		if !modified {
			arr = arr.CloneList()
			modified = true
		}
		arr[i] = out
	}
	if !modified {
		return false, nil
	}
	fgroup = fgroup.CloneObject()
	fgroup["Nodes"] = arr
	return op.sub.Check(st, fgroup)
}

func (op opMergeGroups) Construct(st *State, n nodes.Node) (nodes.Node, error) {
	// TODO(dennwc): implement when we will need a reversal
	//				 see https://github.com/bblfsh/sdk/issues/355
	return op.sub.Construct(st, n)
}
