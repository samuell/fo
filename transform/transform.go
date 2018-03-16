package transform

import (
	"fmt"
	"strings"

	"github.com/albrow/fo/ast"
	"github.com/albrow/fo/astclone"
	"github.com/albrow/fo/astutil"
	"github.com/albrow/fo/token"
	"github.com/albrow/stringset"
)

type usage struct {
	typ    *ast.Ident
	params *ast.ConcreteTypeParamList
}

type declaration struct {
	typ      *ast.Ident
	params   *ast.TypeParamList
	children map[string][]*usage
}

type transformer struct {
	fset       *token.FileSet
	decls      map[string]*declaration
	usages     map[string][]*usage
	seenUsages map[string]stringset.Set
}

// TODO: abstract this out into a usageSet type which does automatic
// de-duplication.
func (trans *transformer) addUsage(usg *usage) {
	if trans.usages == nil {
		trans.usages = map[string][]*usage{}
	}
	if trans.seenUsages == nil {
		trans.seenUsages = map[string]stringset.Set{}
	}
	if usages, found := trans.usages[usg.typ.Name]; found {
		if seen := trans.seenUsages[usg.typ.Name]; seen == nil || !seen.Contains(usg.concreteTypeName()) {
			usages = append(usages, usg)
			trans.usages[usg.typ.Name] = usages
			trans.seenUsages[usg.typ.Name].Add(usg.concreteTypeName())
		}
	} else {
		trans.usages[usg.typ.Name] = []*usage{usg}
		trans.seenUsages[usg.typ.Name] = stringset.New()
		trans.seenUsages[usg.typ.Name].Add(usg.concreteTypeName())
	}
}

func (trans *transformer) addDecl(decl *declaration) {
	if trans.decls == nil {
		trans.decls = map[string]*declaration{}
	}
	trans.decls[decl.typ.Name] = decl
}

func (decl *declaration) addChild(usg *usage) {
	if decl.children == nil {
		decl.children = map[string][]*usage{}
	}
	if usages, found := decl.children[usg.typ.Name]; found {
		usages = append(usages, usg)
		decl.children[usg.typ.Name] = usages
	} else {
		decl.children[usg.typ.Name] = []*usage{usg}
	}
}

func (d *declaration) stringParams() []string {
	result := []string{}
	for _, ident := range d.params.List {
		result = append(result, ident.Name)
	}
	return result
}

func (u *usage) stringParams() []string {
	return parseConcreteTypeParams(u.params)
}

func parseConcreteTypeParams(params *ast.ConcreteTypeParamList) []string {
	result := []string{}
	for _, expr := range params.List {
		switch x := expr.(type) {
		case *ast.Ident:
			result = append(result, x.Name)
		case *ast.SelectorExpr:
			switch y := x.X.(type) {
			case *ast.Ident:
				result = append(result, y.Name+"."+x.Sel.Name)
			default:
				panic(fmt.Errorf("unexpected selector type in concrete type parameter list: (%T) %s", x.X, x.X))
			}
		default:
			panic(fmt.Errorf("unexpected expression type in concrete type parameter list: (%T) %s", expr, expr))
		}
	}
	return result
}

// TODO: this could be optimized
func (u *usage) inheritsParams(parent *declaration) bool {
	usageSet := stringset.NewFromSlice(u.stringParams())
	parentSet := stringset.NewFromSlice(parent.stringParams())
	return len(stringset.Intersect(usageSet, parentSet)) != 0
}

// TODO: this could be optimized
func (u *usage) concreteTypeName() string {
	paramsCopy := []string{}
	for _, p := range u.stringParams() {
		paramsCopy = append(paramsCopy, strings.Replace(p, ".", "_", -1))
	}
	return fmt.Sprintf("%s__%s", u.typ.Name, strings.Join(paramsCopy, "__"))
}

func File(fset *token.FileSet, f *ast.File) (*ast.File, error) {
	trans := &transformer{
		fset: fset,
	}
	trans.parse(f, nil)
	trans.reduceUsages()
	withConcreteTypes := astutil.Apply(f, nil, trans.generateConcreteTypes())
	result := astutil.Apply(withConcreteTypes, nil, trans.useConcreteTypes())
	resultFile, ok := result.(*ast.File)
	if !ok {
		panic(fmt.Errorf("astutil.Apply returned a non-file type: %T", result))
	}

	return resultFile, nil
}

// parse finds declarations and usages that involve generic type parameters.
// It sets the corresponding values of the transformer, effectively initializing
// it.
func (trans *transformer) parse(root ast.Node, parent *declaration) {
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.TypeParams == nil {
				return true
			}
			// There may be nested type parameters inside of the function that refer
			// back to the parameters of the function itself. For example:
			//
			//   func::(T) (b Box::(T)) value() T { return b.val }
			//
			// We handle this by calling findGenericUsage again with the declaration
			// passed in as a parameter. Any usages found inside of the declaration
			// will be assigned as children of it. (Note that this implementation
			// assumes you cannot have nested generic type declarations).
			decl := &declaration{
				typ:    x.Name,
				params: x.TypeParams,
			}
			trans.addDecl(decl)
			if x.Recv != nil {
				trans.parse(x.Recv, decl)
			}
			trans.parse(x.Type, decl)
			trans.parse(x.TypeParams, decl)
			if x.Body != nil {
				trans.parse(x.Body, decl)
			}
			return false
		case *ast.GenDecl:
			if len(x.Specs) != 1 {
				return true
			}
			typeSpec, isTypeSpec := x.Specs[0].(*ast.TypeSpec)
			if !isTypeSpec {
				return true
			}
			structType, isStructType := typeSpec.Type.(*ast.StructType)
			if !isStructType {
				return true
			}
			if structType.TypeParams == nil {
				return true
			}
			// Just like with function declarations, there may be nested type
			// parameters inside of the struct type. We handle them the same way.
			decl := &declaration{
				typ:    typeSpec.Name,
				params: structType.TypeParams,
			}
			trans.addDecl(decl)
			trans.parse(structType.TypeParams, decl)
			if structType.Fields != nil {
				trans.parse(structType.Fields, decl)
			}
			return false
		case *ast.Ident:
			if x.TypeParams == nil {
				return true
			}
			usg := &usage{
				typ:    x,
				params: x.TypeParams,
			}
			if parent != nil && usg.inheritsParams(parent) {
				// If the parent is not nil and this usage inherits some type parameters
				// from the parent, add this usage to the parent's children. We use this
				// parent -> child relationship to generate the appropriate concrete
				// types for both the parent and the child.
				parent.addChild(usg)
			} else {
				// Otherwise, this usage does not inherit from the parent and we add it
				// to the global list of usages.
				trans.addUsage(usg)
			}
			return true
		default:
			return true
		}
	})
}

func (trans *transformer) reduceUsages() {
	// Add all the usages inside parent declarations.
	for usageName, parentUsages := range trans.usages {
		for _, parentUsage := range parentUsages {
			if decl, found := trans.decls[usageName]; found {
				for _, child := range decl.children {
					for _, childUsage := range child {
						parentTypeMappings := createTypeMappings(decl.params, parentUsage.stringParams())
						newUsage := &usage{
							typ:    astclone.Clone(childUsage.typ).(*ast.Ident),
							params: astclone.Clone(childUsage.params).(*ast.ConcreteTypeParamList),
						}
						for _, param := range newUsage.params.List {
							if paramIdent, ok := param.(*ast.Ident); ok {
								if newType, found := parentTypeMappings[paramIdent.Name]; found {
									paramIdent.Name = newType
								}
							}
						}
						trans.addUsage(newUsage)
					}
				}
			}
		}
	}
}

func (trans *transformer) generateConcreteTypes() func(c *astutil.Cursor) bool {
	return func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *ast.GenDecl:
			if len(n.Specs) == 1 {
				if typeSpec, ok := n.Specs[0].(*ast.TypeSpec); ok {
					switch t := typeSpec.Type.(type) {
					case *ast.StructType:
						if t.TypeParams != nil {
							newNodes := createStructTypeNodes(n, trans.usages[typeSpec.Name.Name])
							for _, node := range newNodes {
								c.InsertBefore(node)
							}
							c.Delete()
						}
					}
				}
			}
		case *ast.FuncDecl:
			if n.TypeParams != nil {
				newNodes := createFuncDeclNodes(n, trans.usages[n.Name.Name])
				for _, node := range newNodes {
					c.InsertBefore(node)
				}
				c.Delete()
			}
		case *ast.Ident:
			// if n.TypeParams != nil {
			// 	params := parseConcreteTypeParams(n.TypeParams)
			// 	newName := generateTypeName(n.Name, params)
			// 	c.Replace(ast.NewIdent(newName))
			// }
		}
		return true
	}
}

func (trans *transformer) useConcreteTypes() func(c *astutil.Cursor) bool {
	return func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *ast.Ident:
			if n.TypeParams != nil {
				params := parseConcreteTypeParams(n.TypeParams)
				newName := generateTypeName(n.Name, params)
				c.Replace(ast.NewIdent(newName))
			}
		}
		return true
	}
}

func createStructTypeNodes(genDecl *ast.GenDecl, usages []*usage) []ast.Node {
	newNodes := []ast.Node{}
	typeSpec := genDecl.Specs[0].(*ast.TypeSpec)
	structType := typeSpec.Type.(*ast.StructType)
	for _, usg := range usages {
		mappings := createTypeMappings(structType.TypeParams, usg.stringParams())
		newDecl := replaceIdentsInScope(astclone.Clone(genDecl), mappings).(*ast.GenDecl)
		newTypeSpec := newDecl.Specs[0].(*ast.TypeSpec)
		newStructType := newTypeSpec.Type.(*ast.StructType)
		newStructType.TypeParams = nil
		newTypeSpec.Name = ast.NewIdent(generateTypeName(typeSpec.Name.Name, usg.stringParams()))
		newNodes = append(newNodes, newDecl)
	}
	return newNodes
}

func createFuncDeclNodes(funcDecl *ast.FuncDecl, usages []*usage) []ast.Node {
	newNodes := []ast.Node{}
	for _, usg := range usages {
		mappings := createTypeMappings(funcDecl.TypeParams, usg.stringParams())
		newDecl := replaceIdentsInScope(astclone.Clone(funcDecl), mappings).(*ast.FuncDecl)
		newDecl.Name = ast.NewIdent(generateTypeName(funcDecl.Name.Name, usg.stringParams()))
		newNodes = append(newNodes, newDecl)
	}
	return newNodes
}

// TODO: handle nested scopes here.
func replaceIdentsInScope(n ast.Node, mappings map[string]string) ast.Node {
	return astutil.Apply(n, nil, func(c *astutil.Cursor) bool {
		if ident, ok := c.Node().(*ast.Ident); ok {
			if newName, found := mappings[ident.Name]; found {
				c.Replace(ast.NewIdent(newName))
			}
		}
		return true
	})
}

func generateTypeName(typeName string, params []string) string {
	paramsCopy := []string{}
	for _, p := range params {
		paramsCopy = append(paramsCopy, strings.Replace(p, ".", "_", -1))
	}
	return fmt.Sprintf("%s__%s", typeName, strings.Join(paramsCopy, "__"))
}

func getTypeParamIndex(typeParams *ast.TypeParamList, typeName string) int {
	for i, paramName := range typeParams.List {
		if paramName.Name == typeName {
			return i
		}
	}
	return -1
}

// TODO: change params to concreteTypeParams?
func createTypeMappings(typeParams *ast.TypeParamList, params []string) map[string]string {
	if len(params) != len(typeParams.List) {
		panic(
			fmt.Errorf(
				"%v: wrong number of type parameters (expected %d but got %d)",
				typeParams.Pos(),
				len(typeParams.List),
				len(params),
			),
		)
	}
	mappings := map[string]string{}
	for i, oldIdent := range typeParams.List {
		mappings[oldIdent.Name] = params[i]
	}
	return mappings
}
