package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"

	"os"
	"path"

	"github.com/cznic/cc"
	"github.com/gorgonia/bindgen"
)

var pkgloc string
var apiFile string
var ctxFile string
var hdrfile string
var pkghdr string
var model = bindgen.Model()

func init() {
	gopath := os.Getenv("GOPATH")
	pkgloc = path.Join(gopath, "src/gorgonia.org/cu/dnn")
	apiFile = path.Join(pkgloc, "api.go")
	ctxFile = path.Join(pkgloc, "ctx_api.go")
	hdrfile = "cudnn.h"

	pkghdr = `package cudnn

/* Generated by gencudnn. DO NOT EDIT */

// #include <cudnn_v7.h>
import "C"
`
}

func handleErr(err error) {
	if err != nil {
		panic(err)
	}
}

// yes I know goimports can be imported, but I'm lazy
func goimports(filename string) error {
	cmd := exec.Command("goimports", "-w", filename)
	return cmd.Run()
}

func main() {
	// Step 1: Explore
	// explore(hdrfile, functions, enums, otherTypes)
	// explore(hdrfile, otherTypes)
	// explore(hdrfile, functions)

	// Step 2: generate mappings for this package, then edit them manually
	// 	Specifically, the `ignored` map is edited - things that will be manually written are not removed from the list
	//	Some enum map names may also be changed
	// generateMappings(true)

	// Step 3: generate enums, then edit the file in the dnn package.
	// generateEnums()
	// generateStubs(true)

	// Step 3a: run parse.py to get more sanity

	// Step 4: manual fix for inconsistent names (Spatial Transforms)

	// step 5:
	generateFunctions()

}

func explore(file string, things ...bindgen.FilterFunc) {
	t, err := bindgen.Parse(model, file)
	handleErr(err)
	bindgen.Explore(t, things...)
}

func generateEnums() {
	buf, err := os.OpenFile(path.Join(pkgloc, "enums.go"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	handleErr(err)
	defer buf.Close()
	fmt.Fprintln(buf, pkghdr)

	t, err := bindgen.Parse(model, hdrfile)
	handleErr(err)

	decls, err := bindgen.Get(t, enums)
	handleErr(err)

	for _, d := range decls {
		e := d.(*bindgen.Enum)
		if isIgnored(e.Name) {
			continue
		}
		fmt.Fprintf(buf, "type %v int\nconst (\n", enumMappings[e.Name])

		var names []string
		for _, a := range e.Type.EnumeratorList() {
			cname := string(a.DefTok.S())
			names = append(names, cname)
		}

		lcp := bindgen.LongestCommonPrefix(names...)

		for _, a := range e.Type.EnumeratorList() {
			cname := string(a.DefTok.S())
			enumName := processEnumName(lcp, cname)
			fmt.Fprintf(buf, "%v %v = C.%v\n", enumName, enumMappings[e.Name], cname)
		}
		fmt.Fprint(buf, ")\n")
		fmt.Fprintf(buf, "func (e %v) c() C.%v { return C.%v(e) }\n", enumMappings[e.Name], e.Name, e.Name)
	}
}

// generateStubs creates most of the stubs
func generateStubs(debugMode bool) {
	t, err := bindgen.Parse(model, hdrfile)
	handleErr(err)
	var buf io.WriteCloser
	var fullpath string
	if debugMode {
		filename := "FOO.go"
		fullpath = path.Join(pkgloc, filename)
		buf, err = os.OpenFile(fullpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		handleErr(err)
		fmt.Fprintln(buf, pkghdr)
	}
outer:
	for k, vs := range setFns {
		gotype, ok := ctypes2GoTypes[k]
		if !ok {
			log.Printf("Cannot generate for %q", k)
			continue
		}
		// if we're not in debugging mode, then we should write out to different files per type generated
		// this makes it easier to work on all the TODOs
		if !debugMode {
			filename := gotype + "_gen.go"
			fullpath = path.Join(pkgloc, filename)
			buf, err = os.OpenFile(fullpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			handleErr(err)
			fmt.Fprintln(buf, pkghdr)
		}

		for _, v := range vs {
			if isIgnored(v) {
				log.Printf("Skipped generating for %q", k)
				continue outer
			}
		}

		// get the creation function to "guess" what should be in the struct
		filter := func(decl *cc.Declarator) bool {
			if decl.Type.Kind() != cc.Function {
				return false
			}
			n := bindgen.NameOf(decl)
			for _, v := range vs {
				if n == v {
					return true
				}
			}
			return false
		}
		decls, err := bindgen.Get(t, filter)
		handleErr(err)

		cs := decls[0].(*bindgen.CSignature)
		sig := GoSignature{}
		var create, destroy string
		if creates, ok := creations[k]; ok {
			create = creates[0]
		}
		if destroys, ok := destructions[k]; ok {
			destroy = destroys[0]
		}

		if create == "" || destroy == "" {
			log.Printf("Skipped %v - No Create/Destroy", k)
			continue
		}

		body := Con{
			Ctype:   k,
			GoType:  gotype,
			Create:  create,
			Destroy: destroy,
			Set:     vs,
		}
		sig.Receiver = Param{Type: gotype}

		if _, err = csig2gosig(cs, "*"+gotype, true, &sig); err != nil {
			body.TODO = err.Error()
			log.Print(body.TODO)
		}
		for _, p := range sig.Params {
			body.Params = append(body.Params, p.Name)
			body.ParamType = append(body.ParamType, p.Type)
		}

		sig.Name = fmt.Sprintf("New%v", gotype)
		sig.Receiver = Param{} // Param is set to empty
		constructStructTemplate.Execute(buf, body)

		fmt.Fprintf(buf, "\n%v{ \n", sig)
		if len(vs) > 1 {
			constructionTODOTemplate.Execute(buf, body)
		} else {
			constructionTemplate.Execute(buf, body)
		}
		fmt.Fprintf(buf, "}\n")

		// getters
		retValPos := getRetVal(cs)
		if len(vs) > 1 {
			fmt.Fprintf(buf, "// TODO: Getters for %v\n", gotype)
			goto generateDestructor
		}
		for i, p := range cs.Parameters() {
			if _, ok := retValPos[i]; ok {
				continue
			}
			getterSig := GoSignature{}
			typName := goNameOf(p.Type())

			// receiver - we don;t log - adds to the noise
			if typName == gotype {
				continue
			}

			if typName == "" {
				fmt.Fprintf(buf, "//TODO: %q: Parameter %d Skipped %q of %v - unmapped type\n", cs.Name, i, p.Name(), p.Type())
				continue
			}

			getterSig.Receiver.Name = strings.ToLower(string(gotype[0]))
			getterSig.Receiver.Type = "*" + gotype
			getterSig.Name = strings.Title(p.Name())
			getterSig.RetVals = []Param{{Type: typName}}
			fmt.Fprintf(buf, "\n%v{ return %v.%v }\n", getterSig, getterSig.Receiver.Name, p.Name())
		}

	generateDestructor:
		// destructor
		destructor := GoSignature{}
		destructor.Name = "destroy" + gotype
		destructor.Params = []Param{
			{Name: "obj", Type: gotype, IsPtr: true},
		}
		fmt.Fprintf(buf, "\n%v{", destructor)
		destructTemplate.Execute(buf, body)
		fmt.Fprintf(buf, "}\n")

		if !debugMode {
			buf.Close()
			if err := goimports(fullpath); err != nil {
				log.Printf("Failed to Goimports %q: %v", fullpath, err)
			}
		}
	}

	buf.Close()
	if err := goimports(fullpath); err != nil {
		log.Printf("Failed to Goimports %q: %v", fullpath, err)
	}
}

func generateFunctions() {
	t, err := bindgen.Parse(model, hdrfile)
	handleErr(err)
	filename := "FOO2.go"
	fullpath := path.Join(pkgloc, filename)
	buf, err := os.OpenFile(fullpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	fmt.Fprintln(buf, pkghdr)
	decls, err := bindgen.Get(t, functions)
	handleErr(err)

	for rec, fns := range methods {
		log.Printf("Receiver : %v. Functions: %d", rec, len(fns))
		for _, decl := range decls {
			csig := decl.(*bindgen.CSignature)
			name := csig.Name
			if !inList(name, fns) {
				continue
			}
			if _, ok := ignored[name]; ok {
				continue
			}
			sig := GoSignature{}
			sig.Receiver.Name = string(rec[0])
			sig.Receiver.Type = goNameOfStr(rec)
			sig.Name = fnNameMap[name]

			csig2gosig(csig, "", true, &sig)

			fmt.Fprintf(buf, "%v {} \n", sig)
		}
	}
}