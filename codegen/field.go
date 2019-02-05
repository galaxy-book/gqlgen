package codegen

import (
	"fmt"
	"go/types"
	"reflect"
	"strconv"
	"strings"

	"github.com/99designs/gqlgen/codegen/config"
	"github.com/99designs/gqlgen/codegen/templates"
	"github.com/99designs/gqlgen/internal/code"
	"github.com/pkg/errors"
	"github.com/vektah/gqlparser/ast"
)

type Field struct {
	*ast.FieldDefinition

	TypeReference    *config.TypeReference
	GoFieldType      GoFieldType      // The field type in go, if any
	GoReceiverName   string           // The name of method & var receiver in go, if any
	GoFieldName      string           // The name of the method or var in go, if any
	IsResolver       bool             // Does this field need a resolver
	Args             []*FieldArgument // A list of arguments to be passed to this field
	MethodHasContext bool             // If this is bound to a go method, does the method also take a context
	NoErr            bool             // If this is bound to a go method, does that method have an error as the second argument
	Object           *Object          // A link back to the parent object
	Default          interface{}      // The default value
	Directives       []*Directive
}

func (b *builder) buildField(obj *Object, field *ast.FieldDefinition) (*Field, error) {
	dirs, err := b.getDirectives(field.Directives)
	if err != nil {
		return nil, err
	}

	tr, err := b.Binder.TypeReference(field.Type)
	if err != nil {
		return nil, err
	}

	f := Field{
		FieldDefinition: field,
		TypeReference:   tr,
		Object:          obj,
		Directives:      dirs,
		GoFieldName:     templates.ToGo(field.Name),
		GoFieldType:     GoFieldVariable,
		GoReceiverName:  "obj",
	}

	if field.DefaultValue != nil {
		var err error
		f.Default, err = field.DefaultValue.Value(nil)
		if err != nil {
			return nil, errors.Errorf("default value %s is not valid: %s", field.Name, err.Error())
		}
	}

	typeEntry, entryExists := b.Config.Models[obj.Definition.Name]
	if entryExists {
		if typeField, ok := typeEntry.Fields[field.Name]; ok {
			if typeField.Resolver {
				f.IsResolver = true
			}
			if typeField.FieldName != "" {
				f.GoFieldName = templates.ToGo(typeField.FieldName)
			}
		}
	}

	for _, arg := range field.Arguments {
		newArg, err := b.buildArg(obj, arg)
		if err != nil {
			return nil, err
		}
		f.Args = append(f.Args, newArg)
	}
	return &f, nil
}

func (b *builder) bindMethod(t types.Type, field *Field) error {
	namedType, err := findGoNamedType(t)
	if err != nil {
		return err
	}

	method := b.findMethod(namedType, field.GoFieldName)
	if method == nil {
		return fmt.Errorf("no method named %s", field.GoFieldName)
	}
	sig := method.Type().(*types.Signature)

	if sig.Results().Len() == 1 {
		field.NoErr = true
	} else if sig.Results().Len() != 2 {
		return fmt.Errorf("method has wrong number of args")
	}
	params := sig.Params()
	// If the first argument is the context, remove it from the comparison and set
	// the MethodHasContext flag so that the context will be passed to this model's method
	if params.Len() > 0 && params.At(0).Type().String() == "context.Context" {
		field.MethodHasContext = true
		vars := make([]*types.Var, params.Len()-1)
		for i := 1; i < params.Len(); i++ {
			vars[i-1] = params.At(i)
		}
		params = types.NewTuple(vars...)
	}

	if err := b.bindArgs(field, params); err != nil {
		return err
	}

	result := sig.Results().At(0)
	if err = code.CompatibleTypes(field.TypeReference.GO, result.Type()); err != nil {
		return errors.Wrapf(err, "%s is not compatible with %s", field.TypeReference.GO.String(), result.String())
	}

	// success, args and return type match. Bind to method
	field.GoFieldType = GoFieldMethod
	field.GoReceiverName = "obj"
	field.GoFieldName = method.Name()
	field.TypeReference.GO = result.Type()
	return nil
}

func (b *builder) bindVar(t types.Type, field *Field) error {
	underlying, ok := t.Underlying().(*types.Struct)
	if !ok {
		return fmt.Errorf("not a struct")
	}

	structField, err := b.findField(underlying, field.GoFieldName)
	if err != nil {
		return err
	}

	if err = code.CompatibleTypes(field.TypeReference.GO, structField.Type()); err != nil {
		return errors.Wrapf(err, "%s is not compatible with %s", field.TypeReference.GO.String(), field.TypeReference.GO.String())
	}

	// success, bind to var
	field.GoFieldType = GoFieldVariable
	field.GoReceiverName = "obj"
	field.GoFieldName = structField.Name()
	field.TypeReference.GO = structField.Type()
	return nil
}

// findField attempts to match the name to a struct field with the following
// priorites:
// 1. If struct tag is passed then struct tag has highest priority
// 2. Actual Field name
// 3. Field in an embedded struct
func (b *builder) findField(typ *types.Struct, name string) (*types.Var, error) {
	if b.Config.StructTag != "" {
		var foundField *types.Var
		for i := 0; i < typ.NumFields(); i++ {
			field := typ.Field(i)
			if !field.Exported() {
				continue
			}
			tags := reflect.StructTag(typ.Tag(i))
			if val, ok := tags.Lookup(b.Config.StructTag); ok && equalFieldName(val, name) {
				if foundField != nil {
					return nil, errors.Errorf("tag %s is ambigious; multiple fields have the same tag value of %s", b.Config.StructTag, val)
				}

				foundField = field
			}
		}
		if foundField != nil {
			return foundField, nil
		}
	}

	for i := 0; i < typ.NumFields(); i++ {
		field := typ.Field(i)
		if !field.Exported() {
			continue
		}
		if equalFieldName(field.Name(), name) { // aqui!
			return field, nil
		}
	}

	for i := 0; i < typ.NumFields(); i++ {
		field := typ.Field(i)
		if !field.Exported() {
			continue
		}

		if field.Anonymous() {
			fieldType := field.Type()

			if ptr, ok := fieldType.(*types.Pointer); ok {
				fieldType = ptr.Elem()
			}

			// Type.Underlying() returns itself for all types except types.Named, where it returns a struct type.
			// It should be safe to always call.
			if named, ok := fieldType.Underlying().(*types.Struct); ok {
				f, err := b.findField(named, name)
				if err != nil && !strings.HasPrefix(err.Error(), "no field named") {
					return nil, err
				}
				if f != nil {
					return f, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no field named %s", name)
}

func (b *builder) findMethod(typ *types.Named, name string) *types.Func {
	for i := 0; i < typ.NumMethods(); i++ {
		method := typ.Method(i)
		if !method.Exported() {
			continue
		}

		if strings.EqualFold(method.Name(), name) {
			return method
		}
	}

	if s, ok := typ.Underlying().(*types.Struct); ok {
		for i := 0; i < s.NumFields(); i++ {
			field := s.Field(i)
			if !field.Anonymous() {
				continue
			}

			if named, ok := field.Type().(*types.Named); ok {
				if f := b.findMethod(named, name); f != nil {
					return f
				}
			}
		}
	}

	return nil
}

func (f *Field) HasDirectives() bool {
	return len(f.Directives) > 0
}

func (f *Field) IsReserved() bool {
	return strings.HasPrefix(f.Name, "__")
}

func (f *Field) IsMethod() bool {
	return f.GoFieldType == GoFieldMethod
}

func (f *Field) IsVariable() bool {
	return f.GoFieldType == GoFieldVariable
}

func (f *Field) IsConcurrent() bool {
	if f.Object.DisableConcurrency {
		return false
	}
	return f.MethodHasContext || f.IsResolver
}

func (f *Field) GoNameUnexported() string {
	return templates.ToGoPrivate(f.Name)
}

func (f *Field) ShortInvocation() string {
	return fmt.Sprintf("%s().%s(%s)", f.Object.Definition.Name, f.GoFieldName, f.CallArgs())
}

func (f *Field) ArgsFunc() string {
	if len(f.Args) == 0 {
		return ""
	}

	return "field_" + f.Object.Definition.Name + "_" + f.Name + "_args"
}

func (f *Field) ResolverType() string {
	if !f.IsResolver {
		return ""
	}

	return fmt.Sprintf("%s().%s(%s)", f.Object.Definition.Name, f.GoFieldName, f.CallArgs())
}

func (f *Field) ShortResolverDeclaration() string {
	if !f.IsResolver {
		return ""
	}
	res := fmt.Sprintf("%s(ctx context.Context", f.GoFieldName)

	if !f.Object.Root {
		res += fmt.Sprintf(", obj *%s", templates.CurrentImports.LookupType(f.Object.Type))
	}
	for _, arg := range f.Args {
		res += fmt.Sprintf(", %s %s", arg.VarName, templates.CurrentImports.LookupType(arg.TypeReference.GO))
	}

	result := templates.CurrentImports.LookupType(f.TypeReference.GO)
	if f.Object.Stream {
		result = "<-chan " + result
	}

	res += fmt.Sprintf(") (%s, error)", result)
	return res
}

func (f *Field) ComplexitySignature() string {
	res := fmt.Sprintf("func(childComplexity int")
	for _, arg := range f.Args {
		res += fmt.Sprintf(", %s %s", arg.VarName, templates.CurrentImports.LookupType(arg.TypeReference.GO))
	}
	res += ") int"
	return res
}

func (f *Field) ComplexityArgs() string {
	var args []string
	for _, arg := range f.Args {
		args = append(args, "args["+strconv.Quote(arg.Name)+"].("+templates.CurrentImports.LookupType(arg.TypeReference.GO)+")")
	}

	return strings.Join(args, ", ")
}

func (f *Field) CallArgs() string {
	var args []string

	if f.IsResolver {
		args = append(args, "rctx")

		if !f.Object.Root {
			args = append(args, "obj")
		}
	} else {
		if f.MethodHasContext {
			args = append(args, "ctx")
		}
	}

	for _, arg := range f.Args {
		args = append(args, "args["+strconv.Quote(arg.Name)+"].("+templates.CurrentImports.LookupType(arg.TypeReference.GO)+")")
	}

	return strings.Join(args, ", ")
}
