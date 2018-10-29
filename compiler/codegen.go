package compiler

import (
	"bytes"
	"encoding/binary"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log"
	"strconv"
	"strings"

	"github.com/CityOfZion/neo-go/pkg/crypto"
	"github.com/CityOfZion/neo-storm/vm"
)

// The identifier of the entry function. Default set to Main.
const mainIdent = "Main"

type codegen struct {
	// Information about the program with all its dependencies.
	buildInfo *buildInfo

	// prog holds the output buffer
	prog *bytes.Buffer

	// Type information
	typeInfo *types.Info

	// A mapping of func identifiers with their scope.
	funcs map[string]*funcScope

	// Current funcScope being converted.
	scope *funcScope

	// Label table for recording jump destinations.
	l []int
}

// newLabel creates a new label to jump to
func (c *codegen) newLabel() (l int) {
	l = len(c.l)
	c.l = append(c.l, -1)
	return
}

func (c *codegen) setLabel(l int) {
	c.l[l] = c.pc() + 1
}

// pc return the program offset off the last instruction.
func (c *codegen) pc() int {
	return c.prog.Len() - 1
}

func (c *codegen) emitLoadConst(t types.TypeAndValue) {
	switch typ := t.Type.Underlying().(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Int, types.UntypedInt, types.Uint:
			val, _ := constant.Int64Val(t.Value)
			emitInt(c.prog, val)
		case types.String, types.UntypedString:
			val := constant.StringVal(t.Value)
			emitString(c.prog, val)
		case types.Bool, types.UntypedBool:
			val := constant.BoolVal(t.Value)
			emitBool(c.prog, val)
		case types.Byte:
			val, _ := constant.Int64Val(t.Value)
			b := byte(val)
			emitBytes(c.prog, []byte{b})
		default:
			log.Fatalf("compiler don't know how to convert this basic type: %v", t)
		}
	default:
		log.Fatalf("compiler don't know how to convert this constant: %v", t)
	}
}

func (c *codegen) emitLoadLocal(name string) {
	pos := c.scope.loadLocal(name)
	if pos < 0 {
		log.Fatalf("cannot load local variable with position: %d", pos)
	}
	c.emitLoadLocalPos(pos)
}

func (c *codegen) emitLoadLocalPos(pos int) {
	emitOpcode(c.prog, vm.DUPFROMALTSTACK)
	emitInt(c.prog, int64(pos))
	emitOpcode(c.prog, vm.PICKITEM)
}

func (c *codegen) emitStoreLocal(pos int) {
	emitOpcode(c.prog, vm.DUPFROMALTSTACK)

	if pos < 0 {
		log.Fatalf("invalid position to store local: %d", pos)
	}

	emitInt(c.prog, int64(pos))
	emitInt(c.prog, 2)
	emitOpcode(c.prog, vm.ROLL)
	emitOpcode(c.prog, vm.SETITEM)
}

func (c *codegen) emitLoadField(i int) {
	emitInt(c.prog, int64(i))
	emitOpcode(c.prog, vm.PICKITEM)
}

func (c *codegen) emitStoreStructField(i int) {
	emitInt(c.prog, int64(i))
	emitOpcode(c.prog, vm.ROT)
	emitOpcode(c.prog, vm.SETITEM)
}

// convertGlobals will traverse the AST and only convert global declarations.
// If we call this in convertFuncDecl then it will load all global variables
// into the scope of the function.
func (c *codegen) convertGlobals(f *ast.File) {
	ast.Inspect(f, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			return false
		case *ast.GenDecl:
			ast.Walk(c, n)
		}
		return true
	})
}

func (c *codegen) convertFuncDecl(file *ast.File, decl *ast.FuncDecl) {
	var (
		f  *funcScope
		ok bool
	)

	f, ok = c.funcs[decl.Name.Name]
	if ok {
		// If this function is a syscall we will not convert it to bytecode.
		if isSyscall(f) {
			return
		}
		c.setLabel(f.label)
	} else {
		f = c.newFunc(decl)
	}

	c.scope = f
	ast.Inspect(decl, c.scope.analyzeVoidCalls) // @OPTIMIZE

	// All globals copied into the scope of the function need to be added
	// to the stack size of the function.
	emitInt(c.prog, f.stackSize()+countGlobals(file))
	emitOpcode(c.prog, vm.NEWARRAY)
	emitOpcode(c.prog, vm.TOALTSTACK)

	// We need to handle methods, which in Go, is just syntactic sugar.
	// The method receiver will be passed in as first argument.
	// We check if this declaration has a receiver and load it into scope.
	//
	// FIXME: For now we will hard cast this to a struct. We can later finetune this
	// to support other types.
	if decl.Recv != nil {
		for _, arg := range decl.Recv.List {
			ident := arg.Names[0]
			// Currently only method receives for struct types is supported.
			_, ok := c.typeInfo.Defs[ident].Type().Underlying().(*types.Struct)
			if !ok {
				log.Fatal("method receives for non-struct types is not yet supported")
			}
			l := c.scope.newLocal(ident.Name)
			c.emitStoreLocal(l)
		}
	}

	// Load the arguments in scope.
	for _, arg := range decl.Type.Params.List {
		name := arg.Names[0].Name // for now.
		l := c.scope.newLocal(name)
		c.emitStoreLocal(l)
	}
	// Load in all the global variables in to the scope of the function.
	// This is not necessary for syscalls.
	if !isSyscall(f) {
		c.convertGlobals(file)
	}

	ast.Walk(c, decl.Body)

	// If this function returns the void (no return stmt) we will cleanup its junk on the stack.
	if !hasReturnStmt(decl) {
		emitOpcode(c.prog, vm.FROMALTSTACK)
		emitOpcode(c.prog, vm.DROP)
		emitOpcode(c.prog, vm.RET)
	}
}

func (c *codegen) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {

	// General declarations.
	// var (
	//     x = 2
	// )
	case *ast.GenDecl:
		for _, spec := range n.Specs {
			switch t := spec.(type) {
			case *ast.ValueSpec:
				for i, val := range t.Values {
					ast.Walk(c, val)
					l := c.scope.newLocal(t.Names[i].Name)
					c.emitStoreLocal(l)
				}
			}
		}
		return nil

	case *ast.AssignStmt:
		for i := 0; i < len(n.Lhs); i++ {
			switch t := n.Lhs[i].(type) {
			case *ast.Ident:
				switch n.Tok {
				case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN:
					c.emitLoadLocal(t.Name)
					ast.Walk(c, n.Rhs[0]) // can only add assign to 1 expr on the RHS
					c.convertToken(n.Tok)
					l := c.scope.loadLocal(t.Name)
					c.emitStoreLocal(l)
				default:
					ast.Walk(c, n.Rhs[i])
					l := c.scope.loadLocal(t.Name)
					c.emitStoreLocal(l)
				}

			case *ast.SelectorExpr:
				switch expr := t.X.(type) {
				case *ast.Ident:
					ast.Walk(c, n.Rhs[i])
					typ := c.typeInfo.ObjectOf(expr).Type().Underlying()
					if strct, ok := typ.(*types.Struct); ok {
						c.emitLoadLocal(expr.Name)            // load the struct
						i := indexOfStruct(strct, t.Sel.Name) // get the index of the field
						c.emitStoreStructField(i)             // store the field
					}
				default:
					log.Fatal("nested selector assigns not supported yet")
				}

			// Assignments to index expressions.
			// slice[0] = 10
			case *ast.IndexExpr:
				ast.Walk(c, n.Rhs[i])
				name := t.X.(*ast.Ident).Name
				c.emitLoadLocal(name)
				// For now storm only supports basic index operations. Hence we
				// cast this to an *ast.BasicLit (1, 2 , 3)
				indexStr := t.Index.(*ast.BasicLit).Value
				index, err := strconv.Atoi(indexStr)
				if err != nil {
					log.Fatal("failed to convert slice index to integer")
				}
				c.emitStoreStructField(index)
			}
		}
		return nil

	case *ast.ReturnStmt:
		if len(n.Results) > 1 {
			log.Fatal("multiple returns not supported.")
		}

		l := c.newLabel()
		c.setLabel(l)

		if len(n.Results) > 0 {
			ast.Walk(c, n.Results[0])
		}

		emitOpcode(c.prog, vm.FROMALTSTACK)
		emitOpcode(c.prog, vm.DROP) // Cleanup the stack.
		emitOpcode(c.prog, vm.RET)
		return nil

	case *ast.IfStmt:
		lIf := c.newLabel()
		lElse := c.newLabel()
		lElseEnd := c.newLabel()

		if n.Cond != nil {
			ast.Walk(c, n.Cond)
			emitJmp(c.prog, vm.JMPIFNOT, int16(lElse))
		}

		c.setLabel(lIf)
		ast.Walk(c, n.Body)
		if n.Else != nil {
			emitJmp(c.prog, vm.JMP, int16(lElseEnd))
		}

		c.setLabel(lElse)
		if n.Else != nil {
			ast.Walk(c, n.Else)
		}
		c.setLabel(lElseEnd)
		return nil

	case *ast.BasicLit:
		c.emitLoadConst(c.typeInfo.Types[n])
		return nil

	case *ast.Ident:
		if isIdentBool(n) {
			c.emitLoadConst(makeBoolFromIdent(n, c.typeInfo))
		} else {
			c.emitLoadLocal(n.Name)
		}
		return nil

	case *ast.CompositeLit:
		var typ types.Type

		switch t := n.Type.(type) {
		case *ast.Ident:
			typ = c.typeInfo.ObjectOf(t).Type().Underlying()
		case *ast.SelectorExpr:
			typ = c.typeInfo.ObjectOf(t.Sel).Type().Underlying()
		default:
			ln := len(n.Elts)
			// ByteArrays need a different approach then normal arrays.
			if isByteArray(n, c.typeInfo) {
				c.convertByteArray(n)
				return nil
			}
			for i := ln - 1; i >= 0; i-- {
				c.emitLoadConst(c.typeInfo.Types[n.Elts[i]])
			}
			emitInt(c.prog, int64(ln))
			emitOpcode(c.prog, vm.PACK)
			return nil
		}

		switch typ.(type) {
		case *types.Struct:
			c.convertStruct(n)
		}

		return nil

	case *ast.BinaryExpr:
		switch n.Op {
		case token.LAND:
			ast.Walk(c, n.X)
			emitJmp(c.prog, vm.JMPIFNOT, int16(len(c.l)-1))
			ast.Walk(c, n.Y)
			return nil

		case token.LOR:
			ast.Walk(c, n.X)
			emitJmp(c.prog, vm.JMPIF, int16(len(c.l)-2))
			ast.Walk(c, n.Y)
			return nil

		default:
			// The AST package will try to resolve all basic literals for us.
			// If the typeinfo.Value is not nil we know that the expr is resolved
			// and needs no further action. e.g. x := 2 + 2 + 2 will be resolved to 6.
			// NOTE: Constants will also be automagically resolved be the AST parser.
			// example:
			// const x = 10
			// x + 2 will results into 12
			tinfo := c.typeInfo.Types[n]
			if tinfo.Value != nil {
				c.emitLoadConst(tinfo)
				return nil
			}

			ast.Walk(c, n.X)
			ast.Walk(c, n.Y)

			// VM has separate opcode for string concatenation
			if n.Op == token.ADD {
				typ, ok := tinfo.Type.Underlying().(*types.Basic)
				if ok && typ.Kind() == types.String {
					emitOpcode(c.prog, vm.CAT)
				} else {
					emitOpcode(c.prog, vm.ADD)
				}
			} else {
				c.convertToken(n.Op)
			}
			return nil
		}

	case *ast.CallExpr:
		var (
			f         *funcScope
			ok        bool
			numArgs   = len(n.Args)
			isBuiltin = isBuiltin(n.Fun)
		)

		switch fun := n.Fun.(type) {
		case *ast.Ident:
			f, ok = c.funcs[fun.Name]
			if !ok && !isBuiltin {
				log.Fatalf("could not resolve function %s", fun.Name)
			}
		case *ast.SelectorExpr:
			// If this is a method call we need to walk the AST to load the struct locally.
			// Otherwise this is a function call from a imported package and we can call it
			// directly.
			if c.typeInfo.Selections[fun] != nil {
				ast.Walk(c, fun.X)
				// Dont forget to add 1 extra argument when its a method.
				numArgs++
			}

			f, ok = c.funcs[fun.Sel.Name]
			// @FIXME this could cause runtime errors.
			f.selector = fun.X.(*ast.Ident)
			if !ok {
				log.Fatalf("could not resolve function %s", fun.Sel.Name)
			}
		case *ast.ArrayType:
			// For now we will assume that there is only 1 argument passed which
			// will be a basic literal (string kind). This only to handle string
			// to byte slice conversions. E.G. []byte("foobar")
			arg := n.Args[0].(*ast.BasicLit)
			c.emitLoadConst(c.typeInfo.Types[arg])
			return nil
		}

		// Handle the arguments
		for _, arg := range n.Args {
			ast.Walk(c, arg)
		}
		// Do not swap for builtin functions.
		if !isBuiltin {
			if numArgs == 2 {
				emitOpcode(c.prog, vm.SWAP)
			}
			if numArgs == 3 {
				emitInt(c.prog, 2)
				emitOpcode(c.prog, vm.XSWAP)
			}
			if numArgs > 3 {
				for i := 1; i < numArgs; i++ {
					emitInt(c.prog, int64(i))
					emitOpcode(c.prog, vm.ROLL)
				}
			}
		}

		// Check builtin first to avoid nil pointer on funcScope!
		if isBuiltin {
			// Use the ident to check, builtins are not in func scopes.
			// We can be sure builtins are of type *ast.Ident.
			c.convertBuiltin(n)
		} else if isSyscall(f) {
			c.convertSyscall(f.selector.Name, f.name)
		} else {
			emitCall(c.prog, vm.CALL, int16(f.label))
		}

		return nil

	case *ast.SelectorExpr:
		switch t := n.X.(type) {
		case *ast.Ident:
			typ := c.typeInfo.ObjectOf(t).Type().Underlying()
			if strct, ok := typ.(*types.Struct); ok {
				c.emitLoadLocal(t.Name) // load the struct
				i := indexOfStruct(strct, n.Sel.Name)
				c.emitLoadField(i) // load the field
			}
		default:
			log.Fatal("nested selectors not supported yet")
		}
		return nil

	case *ast.UnaryExpr:
		ast.Walk(c, n.X)
		c.convertToken(n.Op)
		return nil

	case *ast.IncDecStmt:
		ast.Walk(c, n.X)
		c.convertToken(n.Tok)

		// For now only identifiers are supported for (post) for stmts.
		// for i := 0; i < 10; i++ {}
		// Where the post stmt is ( i++ )
		if ident, ok := n.X.(*ast.Ident); ok {
			pos := c.scope.loadLocal(ident.Name)
			c.emitStoreLocal(pos)
		}
		return nil

	case *ast.IndexExpr:
		// Walk the expression, this could be either an Ident or SelectorExpr.
		// This will load local whatever X is.
		ast.Walk(c, n.X)

		switch n.Index.(type) {
		case *ast.BasicLit:
			t := c.typeInfo.Types[n.Index]
			val, _ := constant.Int64Val(t.Value)
			c.emitLoadField(int(val))
		default:
			ast.Walk(c, n.Index)
			emitOpcode(c.prog, vm.PICKITEM) // just pickitem here
		}
		return nil

	case *ast.ForStmt:
		var (
			fstart = c.newLabel()
			fend   = c.newLabel()
		)

		// Walk the initializer and condition.
		ast.Walk(c, n.Init)

		// Set label and walk the condition.
		c.setLabel(fstart)
		ast.Walk(c, n.Cond)

		// Jump if the condition is false
		emitJmp(c.prog, vm.JMPIFNOT, int16(fend))

		// Walk body followed by the iterator (post stmt).
		ast.Walk(c, n.Body)
		ast.Walk(c, n.Post)

		// Jump back to condition.
		emitJmp(c.prog, vm.JMP, int16(fstart))
		c.setLabel(fend)

		return nil

	// We dont really care about assertions for the core logic.
	// The only thing we need is to please the compiler type checking.
	// For this to work properly, we only need to walk the expression
	// not the assertion type.
	case *ast.TypeAssertExpr:
		ast.Walk(c, n.X)
		return nil
	}
	return c
}

func (c *codegen) convertSyscall(api, name string) {
	api, ok := syscalls[api][name]
	if !ok {
		log.Fatalf("unknown VM syscall api: %s", name)
	}
	emitSyscall(c.prog, api)

	// This NOP instruction is basically not needed, but if we do, we have a
	// one to one matching avm file with neo-python which is very nice for debugging.
	emitOpcode(c.prog, vm.NOP)
}

func (c *codegen) convertBuiltin(expr *ast.CallExpr) {
	var name string
	switch t := expr.Fun.(type) {
	case *ast.Ident:
		name = t.Name
	case *ast.SelectorExpr:
		name = t.Sel.Name
	}

	switch name {
	case "len":
		arg := expr.Args[0]
		typ := c.typeInfo.Types[arg].Type
		if isStringType(typ) {
			emitOpcode(c.prog, vm.SIZE)
		} else {
			emitOpcode(c.prog, vm.ARRAYSIZE)
		}
	case "append":
		emitOpcode(c.prog, vm.APPEND)
	case "SHA256":
		emitOpcode(c.prog, vm.SHA256)
	case "SHA1":
		emitOpcode(c.prog, vm.SHA1)
	case "Hash256":
		emitOpcode(c.prog, vm.HASH256)
	case "Hash160":
		emitOpcode(c.prog, vm.HASH160)
	case "Equals":
		emitOpcode(c.prog, vm.EQUAL)
	case "FromAddress":
		// We can be sure that this is a ast.BasicLit just containing a simple
		// address string. Note that the string returned from calling Value will
		// contain double qoutes that need to be stripped.
		addressStr := expr.Args[0].(*ast.BasicLit).Value
		addressStr = strings.Replace(addressStr, "\"", "", 2)
		uint160, err := crypto.Uint160DecodeAddress(addressStr)
		if err != nil {
			log.Fatal(err)
		}
		bytes := uint160.Bytes()
		emitBytes(c.prog, bytes)
	}
}

func (c *codegen) convertByteArray(lit *ast.CompositeLit) {
	buf := make([]byte, len(lit.Elts))
	for i := 0; i < len(lit.Elts); i++ {
		t := c.typeInfo.Types[lit.Elts[i]]
		val, _ := constant.Int64Val(t.Value)
		buf[i] = byte(val)
	}
	emitBytes(c.prog, buf)
}

func (c *codegen) convertStruct(lit *ast.CompositeLit) {
	// Create a new structScope to initialize and store
	// the positions of its variables.
	strct, ok := c.typeInfo.TypeOf(lit).Underlying().(*types.Struct)
	if !ok {
		log.Fatalf("the given literal is not of type struct: %v", lit)
	}

	emitOpcode(c.prog, vm.NOP)
	emitInt(c.prog, int64(strct.NumFields()))
	emitOpcode(c.prog, vm.NEWSTRUCT)
	emitOpcode(c.prog, vm.TOALTSTACK)

	// We need to locally store all the fields, even if they are not initialized.
	// We will initialize all fields to their "zero" value.
	for i := 0; i < strct.NumFields(); i++ {
		sField := strct.Field(i)
		fieldAdded := false

		// Fields initialized by the program.
		for _, field := range lit.Elts {
			f := field.(*ast.KeyValueExpr)
			fieldName := f.Key.(*ast.Ident).Name

			if sField.Name() == fieldName {
				ast.Walk(c, f.Value)
				pos := indexOfStruct(strct, fieldName)
				c.emitStoreLocal(pos)
				fieldAdded = true
				break
			}
		}
		if fieldAdded {
			continue
		}

		typeAndVal := typeAndValueForField(sField)
		c.emitLoadConst(typeAndVal)
		c.emitStoreLocal(i)
	}
	emitOpcode(c.prog, vm.FROMALTSTACK)
}

func (c *codegen) convertToken(tok token.Token) {
	switch tok {
	case token.ADD_ASSIGN:
		emitOpcode(c.prog, vm.ADD)
	case token.SUB_ASSIGN:
		emitOpcode(c.prog, vm.SUB)
	case token.MUL_ASSIGN:
		emitOpcode(c.prog, vm.MUL)
	case token.QUO_ASSIGN:
		emitOpcode(c.prog, vm.DIV)
	case token.ADD:
		emitOpcode(c.prog, vm.ADD)
	case token.SUB:
		emitOpcode(c.prog, vm.SUB)
	case token.MUL:
		emitOpcode(c.prog, vm.MUL)
	case token.QUO:
		emitOpcode(c.prog, vm.DIV)
	case token.LSS:
		emitOpcode(c.prog, vm.LT)
	case token.LEQ:
		emitOpcode(c.prog, vm.LTE)
	case token.GTR:
		emitOpcode(c.prog, vm.GT)
	case token.GEQ:
		emitOpcode(c.prog, vm.GTE)
	case token.EQL:
		// It seems that (looking to the python compiler) that comparing for
		// equal (==) needs to return the instruction EQUAL. Where comparing
		// (anything) to not equal (!=) needs to use the opcode NUMNOTEQUAL
		// even for comparing strings.
		emitOpcode(c.prog, vm.EQUAL)
	case token.NEQ:
		emitOpcode(c.prog, vm.NUMNOTEQUAL)
	case token.DEC:
		emitOpcode(c.prog, vm.DEC)
	case token.INC:
		emitOpcode(c.prog, vm.INC)
	case token.NOT:
		emitOpcode(c.prog, vm.NOT)
	case token.AND:
		emitOpcode(c.prog, vm.AND)
	case token.OR:
		emitOpcode(c.prog, vm.OR)
	case token.SHL:
		emitOpcode(c.prog, vm.SHL)
	case token.SHR:
		emitOpcode(c.prog, vm.SHR)
	case token.XOR:
		emitOpcode(c.prog, vm.XOR)
	default:
		log.Fatalf("compiler could not convert token: %s", tok)
	}
}

func (c *codegen) newFunc(decl *ast.FuncDecl) *funcScope {
	f := newFuncScope(decl, c.newLabel())
	c.funcs[f.name] = f
	return f
}

// CodeGen is the function that compiles the program to bytecode.
func CodeGen(info *buildInfo) (*bytes.Buffer, error) {
	pkg := info.program.Package(info.initialPackage)
	c := &codegen{
		buildInfo: info,
		prog:      new(bytes.Buffer),
		l:         []int{},
		funcs:     map[string]*funcScope{},
		typeInfo:  &pkg.Info,
	}

	// Resolve the entrypoint of the program
	main, mainFile := resolveEntryPoint(mainIdent, pkg)
	if main == nil {
		log.Fatal("could not find func main. did you forgot to declare it?")
	}

	funUsage := analyzeFuncUsage(info.program.AllPackages)

	// Bring all imported functions into scope
	for _, pkg := range info.program.AllPackages {
		for _, f := range pkg.Files {
			c.resolveFuncDecls(f)
		}
	}

	// convert the entry point first
	c.convertFuncDecl(mainFile, main)

	// Generate the code for the program
	for _, pkg := range info.program.AllPackages {
		c.typeInfo = &pkg.Info

		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				switch n := decl.(type) {
				case *ast.FuncDecl:
					// Dont convert the function if its not used. This will save alot
					// of bytecode space.
					if n.Name.Name != mainIdent && funUsage.funcUsed(n.Name.Name) {
						c.convertFuncDecl(f, n)
					}
				}
			}
		}
	}

	c.writeJumps()

	return c.prog, nil
}

func (c *codegen) resolveFuncDecls(f *ast.File) {
	for _, decl := range f.Decls {
		switch n := decl.(type) {
		case *ast.FuncDecl:
			if n.Name.Name != mainIdent {
				c.newFunc(n)
			}
		}
	}
}

func (c *codegen) writeJumps() {
	b := c.prog.Bytes()
	for i, op := range b {
		j := i + 1
		switch vm.Instruction(op) {
		case vm.JMP, vm.JMPIFNOT, vm.JMPIF, vm.CALL:
			index := int16(binary.LittleEndian.Uint16(b[j : j+2]))
			if int(index) > len(c.l) || int(index) < 0 {
				continue
			}
			offset := uint16(c.l[index] - i)
			if offset < 0 {
				log.Fatalf("new offset is negative, table list %v", c.l)
			}
			binary.LittleEndian.PutUint16(b[j:j+2], offset)
		}
	}
}
