package lsp

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"net"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/internal/jsonrpc2"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
)

// RunElasticServer starts an LSP server on the supplied stream, and waits until the
// stream is closed.
func RunElasticServer(ctx context.Context, stream jsonrpc2.Stream, opts ...interface{}) error {
	s := &elasticserver{}
	conn, client := protocol.RunElasticServer(ctx, stream, s, opts...)
	s.client = client
	return conn.Wait(ctx)
}

// RunElasticServerOnPort starts an LSP server on the given port and does not exit.
// This function exists for debugging purposes.
func RunElasticServerOnPort(ctx context.Context, port int, opts ...interface{}) error {
	return RunElasticServerOnAddress(ctx, fmt.Sprintf(":%v", port))
}

// RunElasticServerOnAddress starts an LSP server on the given port and does not exit.
// This function exists for debugging purposes.
func RunElasticServerOnAddress(ctx context.Context, addr string, opts ...interface{}) error {
	s := &elasticserver{}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		stream := jsonrpc2.NewHeaderStream(conn, conn)
		go func() {
			conn, client := protocol.RunElasticServer(ctx, stream, s, opts...)
			s.client = client
			conn.Wait(ctx)
		}()
	}
}

// elasticserver "inherits" from lsp.server and is used to implement the elastic extention for the official go lsp.
type elasticserver struct {
	server
}

// EDefinition has almost the same functionality with Definition except for the qualified name and symbol kind.
func (s *elasticserver) EDefinition(ctx context.Context, params *protocol.TextDocumentPositionParams) ([]protocol.SymbolLocator, error) {
	sourceURI, err := fromProtocolURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}

	f, err := s.view.GetFile(ctx, sourceURI)
	if err != nil {
		return nil, err
	}
	tok := f.GetToken(ctx)

	// Note GetToken may return nil, check the return value before the access. See https://github.com/golang/go/issues/30562
	if tok == nil {
		return nil, fmt.Errorf("no token files found for this file")
	}

	pos := fromProtocolPosition(tok, params.Position)
	ident, err := source.Identifier(ctx, s.view, f, pos)
	if err != nil {
		return nil, err
	}

	kind := getSymbolKind(ident)
	if kind == 0 {
		return nil, fmt.Errorf("no corresponding symbol kind for '" + ident.Name + "'")
	}

	qname := getQName(ctx, f, ident, kind)

	// Get the package where the symbol belongs to.
	pkg := ident.Declaration.Object.Pkg()
	if pkg == nil {
		return nil, fmt.Errorf("no packages found for the identifier")
	}

	var pkgLoc protocol.PackageLocator
	if pkg.Name() != f.GetPackage(ctx).Name {
		// Handle the case where symbols imported from other packages.
		var pkgPath string

		for _, p := range f.GetPackage(ctx).Imports {
			if p.Name == pkg.Name() {
				pkgPath = p.PkgPath
				break
			}
		}

		pkgLoc = protocol.PackageLocator{Name: pkg.Name(), RepoURI: string(pkgPath)}
	} else {
		pkgLoc = protocol.PackageLocator{Name: pkg.Name(), RepoURI: string(f.GetPackage(ctx).PkgPath)}
	}

	loc := toProtocolLocation(s.view.FileSet(), ident.Declaration.Range)

	path := strings.TrimPrefix(string(sourceURI), "file://")

	return []protocol.SymbolLocator{{Qname: qname, Kind: kind, Path: path, Loc: loc, Package: pkgLoc}}, nil
}

// getSymbolKind get the symbol kind for a single position.
// TODO(henrywong) Once the upstream implement the ‘textDocument/documentSymbol’, we should reconsider this method.
//  Like if we cache all the symbols in a document when we handle the document symbol request, we can get the symbol
//  information from the cache directly.
func getSymbolKind(ident *source.IdentifierInfo) protocol.SymbolKind {
	declObj := ident.Declaration.Object
	switch declObj.(type) {
	case *types.Const:
		return protocol.ConstantSymbol
	case *types.Var:
		v, _ := declObj.(*types.Var)
		if v.IsField() {
			return protocol.FieldSymbol
		}
		return protocol.VariableSymbol
	case *types.Nil:
		return protocol.NullSymbol
	case *types.PkgName:
		return protocol.PackageSymbol
	case *types.Func:
		s, _ := declObj.Type().(*types.Signature)
		if s.Recv() == nil {
			return protocol.FunctionSymbol
		}
		return protocol.MethodSymbol
	case *types.TypeName:
		tyObj := ident.Type.Object
		if tyObj != nil {
			namedTy := tyObj.Type().(*types.Named)
			switch namedTy.Underlying().(type) {
			case *types.Struct:
				return protocol.StructSymbol
			case *types.Interface:
				return protocol.InterfaceSymbol
			}
		}
	}

	return protocol.SymbolKind(0)
}

// getQName returns the qualified name for a position in a file. Qualied name mainly served as the cross repo code
// search and code intelligence. The qualified name pattern as bellow:
//  qname = package.name + struct.name* + function.name* | (struct.name + method.name)* + struct.name* + symbol.name
//
// TODO(henrywong) It's better to use the scope chain to give a qualifed name for the symbols, however there is no
// APIs can achieve this goals, just traverse the ast node path for now.
func getQName(ctx context.Context, f source.File, ident *source.IdentifierInfo, kind protocol.SymbolKind) string {
	declObj := ident.Declaration.Object
	qname := declObj.Name()

	if kind == protocol.PackageSymbol {
		return qname
	}

	// Get the file where the symbol definition located.
	fAST := f.GetAST(ctx)
	pos := declObj.Pos()
	path, _ := astutil.PathEnclosingInterval(fAST, pos, pos)

	// TODO(henrywong) Should we put a check here for the case of only one node?
	for id, n := range path[1:] {
		switch n.(type) {
		case *ast.StructType:
			// Check its father to decide whether the ast.StructType is a named type or an anonymous type.
			switch path[id+2].(type) {
			case *ast.TypeSpec:
				// ident is located in a named struct declaration, add the type name into the qualified name.
				ts, _ := path[id+2].(*ast.TypeSpec)
				qname = ts.Name.Name + "." + qname
			case *ast.Field:
				// ident is located in a anonymous struct declaration which used to define a field, like struct fields,
				// function parameters, function named return parameters, add the field name into the qualifed name.
				field, _ := path[id+2].(*ast.Field)
				if len(field.Names) != 0 {
					// If there is a bunch of fields declared with same anonymous struct type, just consider the first field's
					// name.
					qname = field.Names[0].Name + "." + qname
				}

			case *ast.ValueSpec:
				// ident is located in a anonymous struct decalaration which used define a variable, add the variable name into
				// the qualifed name.
				vs, _ := path[id+2].(*ast.ValueSpec)
				if len(vs.Names) != 0 {
					// If there is a bunch of variables declared with same anonymous struct type, just consider the first
					// variable's name.
					qname = vs.Names[0].Name + "." + qname
				}
			}
		case *ast.InterfaceType:
			// Check its father to get the interface name.
			switch path[id+2].(type) {
			case *ast.TypeSpec:
				ts, _ := path[id+2].(*ast.TypeSpec)
				qname = ts.Name.Name + "." + qname
			}

		case *ast.FuncDecl:
			f, _ := n.(*ast.FuncDecl)
			if f.Name != nil && f.Name.Name != qname && (kind == protocol.MethodSymbol || kind == protocol.FunctionSymbol) {
				qname = f.Name.Name + "." + qname
			}

			if f.Name != nil {
				if kind == protocol.MethodSymbol || kind == protocol.FunctionSymbol {
					if f.Name.Name != qname {
						qname = f.Name.Name + "." + qname
					}
				} else {
					qname = f.Name.Name + "." + qname
				}
			}

			// If n is method, add the struct name as a prefix.
			if f.Recv != nil {
				var typeName string
				switch r := f.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					typeName = r.X.(*ast.Ident).Name
				case *ast.Ident:
					typeName = r.Name
				}
				qname = typeName + "." + qname
			}
		case *ast.FuncLit:
			// Considering the function literal is for making the local variable declared in it more unique, the handling is
			// a little tricky. If the function literal is assigned to a named entity, like variable, it is better consider
			// the variable name into the qualified name.

			// Check its ancestors to decide where it is located in, like a assignment, variable declaration, or a return
			// statement.
			switch path[id+2].(type) {
			case *ast.AssignStmt:
				as, _ := path[id+2].(*ast.AssignStmt)
				if i, ok := as.Lhs[0].(*ast.Ident); ok {
					qname = i.Name + "." + qname
				}
			}
		}
	}
	return declObj.Pkg().Name() + "." + qname
}