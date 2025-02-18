package sf

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"golang.org/x/tools/go/analysis"
)

// Configuration variable for including methods (functions with receivers) in the check.
// Set to false to consider only plain functions.
var includeMethods = false

// Run function used in analysis.Analyzer
func Run(pass *analysis.Pass) (any, error) {
	color.NoColor = false

	warningsTotal := 0
	filesTotal := 0
	filesWarned := 0

	for _, file := range pass.Files {
		// Get the filename from the file position.
		filename := pass.Fset.Position(file.Pos()).Filename

		// Skip files that are test files or are in the vendor directory.
		if strings.HasSuffix(filename, "_test.go") || strings.Contains(filepath.ToSlash(filename), "/vendor/") {
			continue
		}

		filesTotal++

		// Walk the AST and look for function declarations.
		var fileContainsWarnings bool
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				if !IsPossibleConverter(fn, pass) {
					return true
				}

				validationResult, err := ValidateConverter(fn, pass)
				if err != nil {
					fmt.Println("--> Validation error, ignoring ", fn.Name.Name)
					return true
				}

				if validationResult.Valid {
					return true
				}

				message := fmt.Sprintf(
					"converter function is leaking fields:\n missing input fields: %v\n missing output fields: %v",
					validationResult.MissingInputFields,
					validationResult.MissingOutputFields,
				)

				var buf bytes.Buffer
				PrettyPrint(&buf, filename, fn, pass, message)

				// Now report the diagnostic using pass.Report.
				pass.Report(analysis.Diagnostic{
					Pos:     fn.Name.Pos(),
					Message: buf.String(),
				})

				warningsTotal++
				fileContainsWarnings = true
			}
			return true
		})
		if fileContainsWarnings {
			filesWarned++
		}
	}

	// At the end of processing all files, print the total number of warnings.
	// Probably temporarily: More for debug purposes.
	// TODO: find a nice way to output reports in linters
	if warningsTotal > 0 {
		fmt.Fprintf(os.Stdout, "\nFiles total analyzed: %d. Warnings: %d caught in %d files\n", filesTotal, warningsTotal, filesWarned)
	} else {
		fmt.Fprintf(os.Stdout, "\nFiles total analyzed: %d. Warnings: 0\n", filesTotal)
	}

	return nil, nil
}

// ContainerType represents the “container” kind for a candidate type.
type ContainerType string

const (
	ContainerNone    ContainerType = "none"    // plain struct
	ContainerPointer ContainerType = "pointer" // pointer to struct
	ContainerSlice   ContainerType = "slice"   // slice or array
	ContainerMap     ContainerType = "map"     // map (using its value type)
)

// candidate holds the underlying candidate type's name and its container type.
type candidate struct {
	name          string
	containerType ContainerType
	structType    *types.Struct
}

// extractCandidateType checks if the given type qualifies as a candidate for conversion.
// It recognizes a plain struct, a pointer to a struct, a slice/array of such types,
// or a map whose value is such a type. If so, it returns the candidate (with its
// underlying type name and container type) and ok==true. Otherwise, ok==false.
func extractCandidateType(t types.Type) (cand candidate, ok bool) {
	// First, check for containers.
	switch tt := t.(type) {
	case *types.Slice, *types.Array:
		cand.containerType = ContainerSlice
		var elem types.Type
		switch x := tt.(type) {
		case *types.Slice:
			elem = x.Elem()
		case *types.Array:
			elem = x.Elem()
		}
		t = elem
	case *types.Map:
		cand.containerType = ContainerMap
		t = tt.Elem()
	default:
		cand.containerType = ContainerNone
	}

	// If the type is a pointer and not already a container, mark it as pointer.
	if ptr, okPtr := t.(*types.Pointer); okPtr {
		if cand.containerType == ContainerNone {
			cand.containerType = ContainerPointer
		}
		t = ptr.Elem()
	}

	// We expect a named type whose underlying type is a struct.
	named, okNamed := t.(*types.Named)
	if !okNamed {
		return candidate{}, false
	}
	st, okStruct := named.Underlying().(*types.Struct)
	if !okStruct {
		return candidate{}, false
	}
	cand.name = named.Obj().Name()
	cand.structType = st
	return cand, true
}

// IsPossibleConverter checks whether fn (a function declaration)
// qualifies as a potential converter function based on these rules:
//   - At least one input and one output candidate exist.
//   - Candidate is the argument who fits the candidate type (struct or pointer to struct).
//   - For at least one candidate pair (input, output) with the same container type,
//     the names of the candidate types share a common substring (ignoring case).
//
// TODO: it can't be the same type e.g. HandleRewrites(sectionRewrites) (string, SectionRewrite, erro)
func IsPossibleConverter(fn *ast.FuncDecl, pass *analysis.Pass) bool {
	// If we're not including methods and this function has a receiver, skip it.
	if !includeMethods && fn.Recv != nil {
		return false
	}

	obj := pass.TypesInfo.Defs[fn.Name]
	if obj == nil {
		return false
	}

	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return false
	}

	// No arguments: nothing was converted
	if sig.Params().Len() == 0 || sig.Results().Len() == 0 {
		return false
	}

	// Gather candidate types from input parameters.
	var inCandidates []candidate
	for i := 0; i < sig.Params().Len(); i++ {
		param := sig.Params().At(i)
		if cand, ok := extractCandidateType(param.Type()); ok {
			inCandidates = append(inCandidates, cand)
		}
	}
	if len(inCandidates) == 0 {
		return false
	}

	// Gather candidate types from output parameters.
	var outCandidates []candidate
	for i := 0; i < sig.Results().Len(); i++ {
		res := sig.Results().At(i)

		if cand, ok := extractCandidateType(res.Type()); ok {
			outCandidates = append(outCandidates, cand)
		}
	}
	if len(outCandidates) == 0 {
		return false
	}

	// Look for at least one candidate pair (in, out) where:
	// - The container types are compatible:
	//    - if the input candidate is a slice or map, then the output candidate must be of the same container type.
	//    - otherwise, if the input candidate is a plain struct or pointer to struct, the output candidate
	//      must also be a plain struct or pointer (i.e. not a slice or map).
	// - And the candidate names share a common substring (ignoring case).
	for _, inCand := range inCandidates {
		lowerIn := strings.ToLower(inCand.name)
		for _, outCand := range outCandidates {
			// Check container type compatibility.
			if inCand.containerType == ContainerSlice || inCand.containerType == ContainerMap {
				if inCand.containerType != outCand.containerType {
					continue // e.g. slice -> non-slice is not allowed.
				}
			} else {
				// inCand is ContainerNone or ContainerPointer.
				// Allow output to be either a plain struct or a pointer.
				if outCand.containerType != ContainerNone && outCand.containerType != ContainerPointer {
					continue
				}
			}

			lowerOut := strings.ToLower(outCand.name)
			if strings.Contains(lowerOut, lowerIn) || strings.Contains(lowerIn, lowerOut) {
				return true
			}
		}
	}

	return false
}

// collectMissingFields is similar to checkAllFieldsUsed but returns a slice of missing field names.
func collectMissingFields(st *types.Struct, usedFields UsageLookup, usedMethodsArg ...UsageLookup) []string {
	var missing []string
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		// adjust this as needed.
		if !field.Exported() {
			continue
		}

		if !usedFields.LookUp(field.Name()) {
			// if methods were given, let's allow via getters
			// If a getter method exists (for input candidate) then allow it.
			if len(usedMethodsArg) > 0 && usedMethodsArg[0].LookUp("Get"+field.Name()) {
				continue
			}
			missing = append(missing, field.Name())
		}
	}
	return missing
}

// ConverterValidationResult holds the details of a converter function validation.
type ConverterValidationResult struct {
	// Valid is true if every exported field (or getter methods) in
	// both input/output models were used.
	Valid bool
	// MissingInputFields contains the names of exported fields in the input candidate
	// that were not used.
	MissingInputFields []string
	// MissingOutputFields contains the names of exported fields in the output candidate
	// that were not used.
	MissingOutputFields []string
}

// ValidateConverter checks that the converter function fn uses every field
// of the candidate input model (by reading) and every field of the candidate output model (by writing).
//
// For input, we assume the candidate comes from the first parameter and that it has a name.
// For output, we first try to use a named result; if none, we look for a composite literal.
func ValidateConverter(fn *ast.FuncDecl, pass *analysis.Pass) (ConverterValidationResult, error) {
	// Retrieve the function object and signature.
	obj := pass.TypesInfo.Defs[fn.Name]
	if obj == nil {
		return ConverterValidationResult{}, fmt.Errorf("cannot get type info for function %q", fn.Name.Name)
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return ConverterValidationResult{}, fmt.Errorf("function %q does not have a valid signature", fn.Name.Name)
	}
	if sig.Params().Len() < 1 || sig.Results().Len() < 1 {
		return ConverterValidationResult{}, fmt.Errorf("function %q must have at least one parameter and one result", fn.Name.Name)
	}

	// Find the candidate input parameter.
	inCand, inVar, okIn := findCandidateParam(fn.Type.Params, sig.Params())
	if !okIn || inVar == "" {
		return ConverterValidationResult{}, fmt.Errorf("cannot determine candidate input parameter for function %q", fn.Name.Name)
	}

	// Determine the candidate output parameter.
	outCand, outVar, okOut := findCandidateParam(fn.Type.Results, sig.Results())
	if !okOut {
		return ConverterValidationResult{}, fmt.Errorf("cannot determine candidate output parameter for function %q", fn.Name.Name)
	}

	// Collect field usages for the input candidate variable.
	fieldsUsedModelIn := CollectUsedFields(fn.Body, inVar)
	methodsUsedModelIn := CollectUsedMethods(fn.Body, inVar)
	missingIn := collectMissingFields(inCand.structType, fieldsUsedModelIn, methodsUsedModelIn)
	for i, m := range missingIn {
		missingIn[i] = inVar + "." + m
	}

	// Collect field usages for the output candidate.
	fieldsUsedModelOut := CollectOutputFields(fn, outVar, outCand.name)
	missingOut := collectMissingFields(outCand.structType, fieldsUsedModelOut)
	if outVar != "" {
		for i, m := range missingOut {
			missingOut[i] = outVar + "." + m
		}
	}

	valid := (len(missingIn) == 0 && len(missingOut) == 0)
	return ConverterValidationResult{
		Valid:               valid,
		MissingInputFields:  missingIn,
		MissingOutputFields: missingOut,
	}, nil
}

// findCandidateParam searches the appropriate FieldList (for input or output)
// for the first parameter/result that qualifies as a candidate type.
// It returns the candidate info, the variable name (if any) and true on success.
func findCandidateParam(fieldList *ast.FieldList, sigParams *types.Tuple) (cand candidate, varName string, found bool) {
	if fieldList == nil {
		return candidate{}, "", false
	}
	// Keep a running count to match the order of parameters/results in sigParams.
	paramIndex := 0
	for _, field := range fieldList.List {
		// A field may declare several names (e.g. "a, b int").
		names := field.Names
		// If no names are present (for results), we still count the parameter.
		n := 1
		if len(names) > 0 {
			n = len(names)
		}
		// For each declared parameter in this field:
		for i := 0; i < n; i++ {
			// Get the type from the signature.
			if paramIndex >= sigParams.Len() {
				break
			}
			paramVar := sigParams.At(paramIndex)
			if c, ok := extractCandidateType(paramVar.Type()); ok {
				// If the AST field has names, use the first one (or the one corresponding to our index).
				if len(names) > 0 {
					return c, names[i].Name, true
				}
				// Otherwise, return with an empty variable name.
				return c, "", true
			}
			paramIndex++
		}
		paramIndex += (n - 1)
	}
	return candidate{}, "", false
}
