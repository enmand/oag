// Package writer writes api Package definitions to files
package writer

import (
	"io"
	"sort"

	"github.com/dave/jennifer/jen"

	"github.com/jbowes/oag/config"
	"github.com/jbowes/oag/pkg"
)

// Write writes the API Package definition p to the provided writer.
func Write(w io.Writer, p *pkg.Package, boilerplate *config.Boilerplate) error {
	f, err := convertPkg(p, boilerplate)
	if err != nil {
		return err
	}

	return f.Render(w)
}

func convertPkg(p *pkg.Package, boilerplate *config.Boilerplate) (*jen.File, error) {
	f := jen.NewFilePathName(p.Qualifier, p.Name)

	f.Comment("This file is automatically generated by oag (https://github.com/jbowes/oag)")
	f.Comment("DO NOT EDIT")
	f.Line()

	if boilerplate.BaseURL != pkg.Disabled {
		f.Const().Id("base" + boilerplate.ClientPrefix + "URL").Op("=").Lit(p.BaseURL)
	}

	for _, d := range p.TypeDecls {
		f.Comment(formatComment(d.Comment))
		td := f.Type().Id(d.Name)
		td.Do(writeType(d.Type))
	}

	for _, iter := range p.Iters {
		defineIter(f, &iter)
	}

	for _, c := range p.Clients {
		f.Comment(c.Comment)
		f.Type().Id(c.Name).Id("endpoint")

		for _, m := range c.Methods {
			convertClientMethod(f, &m, p.TypeDecls)
		}
	}

	if boilerplate.Backend != pkg.Disabled {
		defineBackend(f, boilerplate.ClientPrefix)
	}

	if boilerplate.Endpoint != pkg.Disabled {
		defineEndpoint(f)
	}

	defineClient(f, p.Clients, boilerplate.ClientPrefix)

	return f, nil
}

func convertClientMethod(f *jen.File, m *pkg.Method, decls []pkg.TypeDecl) {
	f.Comment(formatComment(m.Comment))
	fn := f.Func().Params(jen.Id(m.Receiver.ID).Op("*").Id(m.Receiver.Type)).Id(m.Name)

	var fmtArgs, queryArgs, headerArgs []pkg.Param
	var optQueryArgs []pkg.Field

	body := jen.Nil()

	params := []jen.Code{jen.Id("ctx").Qual("context", "Context")}
	for _, p := range m.Params {
		v := jen.Id(p.Arg).Do(writeType(p.Type))

		switch p.Kind {
		case pkg.Path:
			fmtArgs = append(fmtArgs, p)
		case pkg.Query:
			queryArgs = append(queryArgs, p)
		case pkg.Header:
			headerArgs = append(headerArgs, p)
		case pkg.Body:
			body = jen.Id(p.Arg)
		case pkg.Opts:
			for _, d := range decls {
				if d.Name != typeName(p.Type) {
					continue
				}

				sd := d.Type.(*pkg.StructType)
				// XXX handle empty
				for _, f := range sd.Fields {
					switch f.Kind {
					case pkg.Query:
						optQueryArgs = append(optQueryArgs, f)
					default:
						panic("unhandled location for optional arg")
					}
				}
			}
		}

		params = append(params, v)
	}
	fn.Params(params...)

	var rets []jen.Code
	var successRets []jen.Code
	for _, ret := range m.Return {
		v := jen.Do(writeType(ret))
		rets = append(rets, v)

		if _, ok := ret.(*pkg.PointerType); ok {
			successRets = append(successRets, jen.Nil())
		} else if _, ok := ret.(*pkg.IterType); ok {
			successRets = append(successRets, jen.Nil())
		} else if typeName(ret) == "error" {
			successRets = append(successRets, jen.Nil())
		}
	}
	fn.Params(rets...)

	errRet := make([]jen.Code, len(successRets))
	copy(errRet, successRets)
	errRet[len(errRet)-1] = jen.Err()

	fn.BlockFunc(func(g *jen.Group) {
		reqDef := jen.Line()
		reqOp := ":="
		_, iter := m.Return[0].(*pkg.IterType)
		errResp := jen.Err()

		var respDef jen.Code
		var doResp jen.Code = jen.Nil()
		if len(m.Return) > 1 || iter {
			ret := m.Return[0]
			switch t := ret.(type) {
			case *pkg.IterType:
				v := g.Id("iter").Op(":=").Do(writeType(t.Type.(*pkg.PointerType).Type))
				v.Values(jen.Dict{
					jen.Id("i"):     jen.Lit(-1),
					jen.Id("first"): jen.True(),
				})

				g.Line()

				reqDef = reqDef.Var().Id("req").Op("*").Qual("net/http", "Request")
				reqOp = "="

				doResp = jen.Op("&").Id("iter").Dot("page")
				errResp = jen.Id("iter").Dot("err")
				errRet[0] = jen.Op("&").Id("iter")
				successRets[0] = jen.Op("&").Id("iter")
			case *pkg.PointerType:
				respDef = jen.Var().Id("resp").Do(writeType(t.Type))

				doResp = jen.Op("&").Id("resp")
				successRets[0] = doResp
			default:
				respDef = jen.Var().Id("resp").Do(writeType(ret))

				doResp = jen.Id("resp")
				successRets[0] = doResp
			}
		}

		setPathArgs(g, errRet, m.Path, fmtArgs)

		setQueryArgs(g, errRet, queryArgs)
		query := setOptQueryArgs(g, errRet, len(queryArgs) > 0, optQueryArgs)

		g.Add(reqDef)
		g.List(jen.Id("req"), errResp.Clone()).Op(reqOp).Id(m.Receiver.ID).Dot("backend").Dot("NewRequest").Call(
			jen.Qual("net/http", "Method"+m.HTTPMethod),
			jen.Id("p"),
			query,
			body,
		)
		g.If(errResp.Clone().Op("!=").Nil()).BlockFunc(func(g *jen.Group) {
			g.Return(errRet...)
		})
		g.Line()

		setHeaderArgs(g, errRet, headerArgs)

		if respDef != nil {
			g.Add(respDef)
		}
		g.List(jen.Id("_"), errResp.Clone()).Op("=").Id(m.Receiver.ID).Dot("backend").Dot("Do").Call(
			jen.Id("ctx"),
			jen.Id("req"),
			doResp,
			errSelectFunc(m),
		)

		if !iter {
			g.If(errResp.Clone().Op("!=").Nil()).BlockFunc(func(g *jen.Group) {
				g.Return(errRet...)
			})
			g.Line()
		}

		g.Return(successRets...)
	})

}

func setPathArgs(g *jen.Group, errRet []jen.Code, path string, args []pkg.Param) {
	if len(args) == 0 {
		g.Id("p").Op(":=").Lit(path)

		return
	}
	pArgs := []jen.Code{jen.Lit(path)}

	for _, fa := range args {
		if t, ok := fa.Type.(*pkg.IdentType); ok && t.Marshal {
			g.List(jen.Id(fa.ID+"Bytes"), jen.Err()).Op(":=").Id(fa.ID).Dot("MarshalText").Call()
			g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
			pArgs = append(pArgs, jen.String().Params(jen.Id(fa.ID+"Bytes")))
			g.Line()
		} else {
			pArgs = append(pArgs, jen.Id(fa.ID))
		}
	}

	g.Id("p").Op(":=").Qual("fmt", "Sprintf").Call(pArgs...)
}

func setQueryArgs(g *jen.Group, errRet []jen.Code, args []pkg.Param) {
	if len(args) == 0 {
		return
	}

	g.Line()
	g.Id("q").Op(":=").Make(jen.Qual("net/url", "Values"))
	for i, q := range args {
		orig := q.ID
		if q.Orig != "" {
			orig = q.Orig
		}

		switch q.Collection {
		case pkg.None:
			if t, ok := q.Type.(*pkg.IdentType); ok && t.Marshal {
				g.List(jen.Id(q.ID+"Bytes"), jen.Err()).Op(":=").Id(q.ID).Dot("MarshalText").Call()
				g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
				g.Id("q").Dot("Set").Call(jen.Lit(orig), jen.String().Params(jen.Id(q.Arg+"Bytes")))

				if i != len(args)-1 {
					g.Line()
				}
			} else {
				g.Id("q").Dot("Set").Call(jen.Lit(orig), stringFor(q.Type, jen.Id(q.Arg)))
			}
		case pkg.Multi:
			st := q.Type.(*pkg.SliceType)
			g.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id(q.ID)).BlockFunc(func(g *jen.Group) {
				if t, ok := st.Type.(*pkg.IdentType); ok && t.Marshal {
					g.List(jen.Id("b"), jen.Err()).Op(":=").Id("v").Dot("MarshalText").Call()
					g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
					g.Id("q").Dot("Add").Call(jen.Lit(orig), jen.String().Params(jen.Id("b")))
				} else {
					g.Id("q").Dot("Add").Call(jen.Lit(orig), stringFor(st.Type, jen.Id("v")))
				}
			})

			if i != len(args)-1 {
				g.Line()
			}

		default:
			panic("unhandled collection format")
		}
	}
}

func setOptQueryArgs(g *jen.Group, errRet []jen.Code, qDefined bool, args []pkg.Field) jen.Code {
	if len(args) == 0 {
		if !qDefined {
			return jen.Nil()
		}

		return jen.Id("q")
	}

	g.Line()

	init := jen.Empty()
	if !qDefined {
		g.Var().Id("q").Qual("net/url", "Values")
		init = jen.Id("q").Op("=").Make(jen.Qual("net/url", "Values"))
	}

	g.If(jen.Id("opts").Op("!=").Nil()).BlockFunc(func(g *jen.Group) {
		g.Add(init)

		for i, q := range args {
			if i != 0 {
				g.Line()
			}

			orig := q.ID
			if q.Orig != "" {
				orig = q.Orig
			}

			typ := q.Type.(*pkg.PointerType).Type
			switch q.Collection {
			case pkg.None:
				g.If(jen.Id("opts").Dot(q.ID).Op("!=").Nil()).BlockFunc(func(g *jen.Group) {
					if t, ok := typ.(*pkg.IdentType); ok && t.Marshal {
						g.List(jen.Id("b"), jen.Err()).Op(":=").Id("opts").Dot(q.ID).Dot("MarshalText").Call()
						g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
						g.Id("q").Dot("Set").Call(jen.Lit(orig), jen.String().Params(jen.Id("b")))
					} else {
						g.Id("q").Dot("Set").Call(jen.Lit(orig), stringFor(typ, jen.Op("*").Id("opts").Dot(q.ID)))
					}
				})
			case pkg.Multi:
				st := typ.(*pkg.SliceType)
				g.For(jen.List(jen.Id("_"), jen.Id("v")).Op(":=").Range().Id("opts").Dot(q.ID)).BlockFunc(func(g *jen.Group) {
					if t, ok := st.Type.(*pkg.IdentType); ok && t.Marshal {
						g.List(jen.Id("b"), jen.Err()).Op(":=").Id("v").Dot("MarshalText").Call()
						g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
						g.Id("q").Dot("Add").Call(jen.Lit(orig), jen.String().Params(jen.Id("b")))
					} else {
						g.Id("q").Dot("Add").Call(jen.Lit(orig), stringFor(st.Type, jen.Id("v")))
					}
				})
			default:
				panic("unhandled collection format")
			}
		}
	})

	return jen.Id("q")
}

func setHeaderArgs(g *jen.Group, errRet []jen.Code, args []pkg.Param) {
	for i, h := range args {
		orig := h.ID
		if h.Orig != "" {
			orig = h.Orig
		}

		switch h.Collection {
		case pkg.None:
			if t, ok := h.Type.(*pkg.IdentType); ok && t.Marshal {
				g.List(jen.Id(h.Arg+"Bytes"), jen.Err()).Op(":=").Id(h.Arg).Dot("MarshalText").Call()
				g.If(jen.Err().Op("!=").Nil()).Block(jen.Return(errRet...))
				g.Id("req").Dot("Header").Dot("Set").Call(jen.Lit(orig), jen.String().Params(jen.Id(h.Arg+"Bytes")))
				g.Line()
			} else {
				g.Id("req").Dot("Header").Dot("Set").Call(jen.Lit(orig), stringFor(h.Type, jen.Id(h.Arg)))
				if i == len(args)-1 {
					g.Line()
				}
			}
		default:
			panic("unhandled collection format")
		}
	}
}

func errSelectFunc(m *pkg.Method) jen.Code {
	if len(m.Errors) == 0 {
		return jen.Nil()
	}

	var codes []int
	for k := range m.Errors {
		if k != -1 {
			codes = append(codes, k)
		}
	}
	sort.Ints(codes)

	esf := jen.Func().Params(jen.Id("code").Int()).Params(jen.Error())

	switch len(codes) {
	case 0:
		// we already know we have at least one error type, so it must be the default
		t := m.Errors[-1]
		return esf.Block(jen.Return(initType(t)))
	case 1:
		return esf.BlockFunc(func(g *jen.Group) {
			g.If(jen.Id("code").Op("==").Lit(codes[0])).Block(
				jen.Return(initType(m.Errors[codes[0]])),
			)
			g.Return(jen.Nil())
		})
	}

	return esf.BlockFunc(func(g *jen.Group) {
		g.Switch(jen.Id("code")).BlockFunc(func(g *jen.Group) {
			// we know there are at least 2 error codes.
			last := m.Errors[codes[0]]
			cases := []jen.Code{jen.Lit(codes[0])}
			for _, k := range codes[1:] {
				t := m.Errors[k]
				if last.Equal(t) {
					cases = append(cases, jen.Lit(k))
					continue
				}

				g.Case(cases...).Block(jen.Return(initType(last)))

				last = t
				cases = []jen.Code{jen.Lit(k)}
			}
			g.Case(cases...).Block(jen.Return(initType(last)))

			if t, ok := m.Errors[-1]; ok {
				g.Default().Block(jen.Return(initType(t)))
			} else {
				g.Default().Block(jen.Return(jen.Nil()))
			}
		})
	})
}

// stringFor converts basic IdentTypes to  strings
func stringFor(typ pkg.Type, id jen.Code) jen.Code {
	it, ok := typ.(*pkg.IdentType)
	if !ok {
		panic("unknown type for string conversion")
	}

	switch it.Name {
	case "int":
		return jen.Qual("strconv", "Itoa").Call(id)
	case "float64":
		return jen.Qual("strconv", "FormatFloat").Call(id, jen.LitRune('f'), jen.Lit(-1), jen.Lit(64))
	case "bool":
		return jen.Qual("strconv", "FormatBool").Call(id)
	default: // treat as string
		return id
	}
}
