package generator

import (
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"k8s.io/gengo/args"
	"k8s.io/gengo/examples/deepcopy-gen/generators"
)

var (
	blackListTypes = map[string]bool{
		"schema":     true,
		"resource":   true,
		"collection": true,
	}
	underscoreRegexp = regexp.MustCompile(`([a-z])([A-Z])`)
)

func getGoType(field types.Field, schema *types.Schema, schemas *types.Schemas) string {
	return getTypeString(field.Nullable, field.Type, schema, schemas)
}

func getTypeString(nullable bool, typeName string, schema *types.Schema, schemas *types.Schemas) string {
	switch {
	case strings.HasPrefix(typeName, "reference["):
		return "string"
	case strings.HasPrefix(typeName, "map["):
		return "map[string]" + getTypeString(false, typeName[len("map["):len(typeName)-1], schema, schemas)
	case strings.HasPrefix(typeName, "array["):
		return "[]" + getTypeString(false, typeName[len("array["):len(typeName)-1], schema, schemas)
	}

	name := ""

	switch typeName {
	case "json":
		return "interface{}"
	case "boolean":
		name = "bool"
	case "float":
		name = "float64"
	case "int":
		name = "int64"
	case "password":
		return "string"
	case "date":
		return "string"
	case "string":
		return "string"
	case "enum":
		return "string"
	default:
		if schema != nil && schemas != nil {
			otherSchema := schemas.Schema(&schema.Version, typeName)
			if otherSchema != nil {
				name = otherSchema.CodeName
			}
		}

		if name == "" {
			name = convert.Capitalize(typeName)
		}
	}

	if nullable {
		return "*" + name
	}

	return name
}

func getTypeMap(schema *types.Schema, schemas *types.Schemas) map[string]string {
	result := map[string]string{}
	for _, field := range schema.ResourceFields {
		result[field.CodeName] = getGoType(field, schema, schemas)
	}
	return result
}

func getResourceActions(schema *types.Schema, schemas *types.Schemas) map[string]types.Action {
	result := map[string]types.Action{}
	for name, action := range schema.ResourceActions {
		if schemas.Schema(&schema.Version, action.Output) != nil {
			result[name] = action
		}
	}
	return result
}

func generateType(outputDir string, schema *types.Schema, schemas *types.Schemas) error {
	filePath := strings.ToLower("zz_generated_" + addUnderscore(schema.ID) + ".go")
	output, err := os.Create(path.Join(outputDir, filePath))
	if err != nil {
		return err
	}
	defer output.Close()

	typeTemplate, err := template.New("type.template").
		Funcs(funcs()).
		Parse(strings.Replace(typeTemplate, "%BACK%", "`", -1))
	if err != nil {
		return err
	}

	return typeTemplate.Execute(output, map[string]interface{}{
		"schema":          schema,
		"structFields":    getTypeMap(schema, schemas),
		"resourceActions": getResourceActions(schema, schemas),
	})
}

func generateController(outputDir string, schema *types.Schema, schemas *types.Schemas) error {
	filePath := strings.ToLower("zz_generated_" + addUnderscore(schema.ID) + "_controller.go")
	output, err := os.Create(path.Join(outputDir, filePath))
	if err != nil {
		return err
	}
	defer output.Close()

	typeTemplate, err := template.New("controller.template").
		Funcs(funcs()).
		Parse(strings.Replace(controllerTemplate, "%BACK%", "`", -1))
	if err != nil {
		return err
	}

	if schema.InternalSchema != nil {
		schema = schema.InternalSchema
	}

	return typeTemplate.Execute(output, map[string]interface{}{
		"schema":          schema,
		"structFields":    getTypeMap(schema, schemas),
		"resourceActions": getResourceActions(schema, schemas),
	})
}

func generateClient(outputDir string, schemas []*types.Schema) error {
	template, err := template.New("client.template").
		Funcs(funcs()).
		Parse(clientTemplate)
	if err != nil {
		return err
	}

	output, err := os.Create(path.Join(outputDir, "zz_generated_client.go"))
	if err != nil {
		return err
	}
	defer output.Close()

	return template.Execute(output, map[string]interface{}{
		"schemas": schemas,
	})
}

func Generate(schemas *types.Schemas, cattleOutputPackage, k8sOutputPackage string) error {
	baseDir := args.DefaultSourceTree()
	cattleDir := path.Join(baseDir, cattleOutputPackage)
	k8sDir := path.Join(baseDir, k8sOutputPackage)

	if err := prepareDirs(cattleDir, k8sDir); err != nil {
		return err
	}

	generated := []*types.Schema{}
	for _, schema := range schemas.Schemas() {
		if blackListTypes[schema.ID] {
			continue
		}

		if err := generateType(cattleDir, schema, schemas); err != nil {
			return err
		}

		if contains(schema.CollectionMethods, http.MethodGet) {
			if err := generateController(k8sDir, schema, schemas); err != nil {
				return err
			}
		}

		generated = append(generated, schema)
	}

	if err := generateClient(cattleDir, generated); err != nil {
		return err
	}

	if err := deepCopyGen(baseDir, k8sOutputPackage); err != nil {
		return err
	}

	if err := gofmt(baseDir, k8sOutputPackage); err != nil {
		return err
	}

	return gofmt(baseDir, cattleOutputPackage)
}

func prepareDirs(dirs ...string) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}

		files, err := ioutil.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, file := range files {
			if strings.HasPrefix(file.Name(), "zz_generated") {
				if err := os.Remove(path.Join(dir, file.Name())); err != nil {
					return errors.Wrapf(err, "failed to delete %s", path.Join(dir, file.Name()))
				}
			}
		}
	}

	return nil
}

func gofmt(workDir, pkg string) error {
	cmd := exec.Command("goimports", "-w", "-l", "./"+pkg)
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func deepCopyGen(workDir, pkg string) error {
	arguments := &args.GeneratorArgs{
		InputDirs:          []string{pkg},
		OutputBase:         workDir,
		OutputPackagePath:  pkg,
		OutputFileBaseName: "zz_generated_deepcopy",
		GoHeaderFilePath:   "/dev/null",
		GeneratedBuildTag:  "ignore_autogenerated",
	}

	return arguments.Execute(
		generators.NameSystems(),
		generators.DefaultNameSystem(),
		generators.Packages)
}
