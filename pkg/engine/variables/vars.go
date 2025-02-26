package variables

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	kyverno "github.com/kyverno/kyverno/pkg/api/kyverno/v1"
	"github.com/kyverno/kyverno/pkg/engine/anchor/common"
	"github.com/kyverno/kyverno/pkg/engine/context"
	jsonUtils "github.com/kyverno/kyverno/pkg/engine/json-utils"
	"github.com/kyverno/kyverno/pkg/engine/operator"
)

var regexVariables = regexp.MustCompile(`\{\{[^{}]*\}\}`)
var regexReferences = regexp.MustCompile(`\$\(.[^\ ]*\)`)

// IsVariable returns true if the element contains a 'valid' variable {{}}
func IsVariable(value string) bool {
	groups := regexVariables.FindAllStringSubmatch(value, -1)
	return len(groups) != 0
}

// IsReference returns true if the element contains a 'valid' reference $()
func IsReference(value string) bool {
	groups := regexReferences.FindAllStringSubmatch(value, -1)
	return len(groups) != 0
}

// ReplaceAllVars replaces all variables with the value defined in the replacement function
// This is used to avoid validation errors
func ReplaceAllVars(src string, repl func(string) string) string {
	return regexVariables.ReplaceAllStringFunc(src, repl)
}

func SubstituteAll(log logr.Logger, ctx context.EvalInterface, document interface{}) (_ interface{}, err error) {
	document, err = substituteReferences(log, document)
	if err != nil {
		return kyverno.Rule{}, err
	}

	return substituteVars(log, ctx, document)
}

func SubstituteAllForceMutate(log logr.Logger, ctx context.EvalInterface, typedRule kyverno.Rule) (_ kyverno.Rule, err error) {
	var rule interface{}

	rule, err = RuleToUntyped(typedRule)
	if err != nil {
		return kyverno.Rule{}, err
	}

	rule, err = substituteReferences(log, rule)
	if err != nil {
		return kyverno.Rule{}, err
	}

	if ctx == nil {
		rule = replaceSubstituteVariables(rule)
	} else {
		rule, err = substituteVars(log, ctx, rule)
		if err != nil {
			return kyverno.Rule{}, err
		}
	}

	return UntypedToRule(rule)
}

//SubstituteVars replaces the variables with the values defined in the context
// - if any variable is invalid or has nil value, it is considered as a failed variable substitution
func substituteVars(log logr.Logger, ctx context.EvalInterface, rule interface{}) (interface{}, error) {
	return jsonUtils.NewTraversal(rule, substituteVariablesIfAny(log, ctx)).TraverseJSON()
}

func substituteReferences(log logr.Logger, rule interface{}) (interface{}, error) {
	return jsonUtils.NewTraversal(rule, substituteReferencesIfAny(log)).TraverseJSON()
}

// NotFoundVariableErr is returned when it is impossible to resolve the variable
type NotFoundVariableErr struct {
	variable string
	path     string
}

func (n NotFoundVariableErr) Error() string {
	return fmt.Sprintf("variable %s not resolved at path %s", n.variable, n.path)
}

// NotResolvedReferenceErr is returned when it is impossible to resolve the variable
type NotResolvedReferenceErr struct {
	reference string
	path      string
}

func (n NotResolvedReferenceErr) Error() string {
	return fmt.Sprintf("reference %s not resolved at path %s", n.reference, n.path)
}

func substituteReferencesIfAny(log logr.Logger) jsonUtils.Action {
	return jsonUtils.OnlyForLeafs(func(data *jsonUtils.ActionData) (interface{}, error) {
		value, ok := data.Element.(string)
		if !ok {
			return data.Element, nil
		}

		for _, v := range regexReferences.FindAllString(value, -1) {
			resolvedReference, err := resolveReference(log, data.Document, v, data.Path)
			if err != nil {
				switch err.(type) {
				case context.InvalidVariableErr:
					return nil, err
				default:
					return nil, fmt.Errorf("failed to resolve %v at path %s", v, data.Path)
				}
			}

			if resolvedReference == nil {
				return data.Element, fmt.Errorf("failed to resolve %v at path %s", v, data.Path)
			}

			log.V(3).Info("reference resolved", "reference", v, "value", resolvedReference, "path", data.Path)

			if val, ok := resolvedReference.(string); ok {
				value = strings.Replace(value, v, val, -1)
				continue
			}

			return data.Element, NotResolvedReferenceErr{
				reference: v,
				path:      data.Path,
			}
		}

		return value, nil
	})
}

func substituteVariablesIfAny(log logr.Logger, ctx context.EvalInterface) jsonUtils.Action {
	return jsonUtils.OnlyForLeafs(func(data *jsonUtils.ActionData) (interface{}, error) {
		value, ok := data.Element.(string)
		if !ok {
			return data.Element, nil
		}

		originalPattern := value
		vars := regexVariables.FindAllString(value, -1)
		for len(vars) > 0 {
			for _, v := range vars {
				variable := strings.ReplaceAll(v, "{{", "")
				variable = strings.ReplaceAll(variable, "}}", "")
				variable = strings.TrimSpace(variable)

				operation, err := ctx.Query("request.operation")
				if err != nil {
					return nil, fmt.Errorf("failed to check request.operation")
				}

				if operation == "DELETE" {
					variable = strings.ReplaceAll(variable, "request.object", "request.oldObject")
				}

				substitutedVar, err := ctx.Query(variable)
				if err != nil {
					switch err.(type) {
					case context.InvalidVariableErr:
						return nil, err
					default:
						return nil, fmt.Errorf("failed to resolve %v at path %s", variable, data.Path)
					}
				}

				log.V(3).Info("variable substituted", "variable", v, "value", substitutedVar, "path", data.Path)

				if val, ok := substitutedVar.(string); ok {
					value = strings.Replace(value, v, val, -1)
					continue
				}

				if substitutedVar != nil {
					if originalPattern == v {
						return substitutedVar, nil
					}

					return nil, fmt.Errorf("failed to resolve %v at path %s", variable, data.Path)
				}

				return nil, NotFoundVariableErr{
					variable: variable,
					path:     data.Path,
				}
			}

			// check for nested variables in strings
			vars = regexVariables.FindAllString(value, -1)
		}

		return value, nil
	})
}

func resolveReference(log logr.Logger, fullDocument interface{}, reference, absolutePath string) (interface{}, error) {
	var foundValue interface{}

	path := strings.Trim(reference, "$()")

	operation := operator.GetOperatorFromStringPattern(path)
	path = path[len(operation):]

	if len(path) == 0 {
		return nil, errors.New("Expected path. Found empty reference")
	}

	path = formAbsolutePath(path, absolutePath)

	valFromReference, err := getValueFromReference(fullDocument, path)
	if err != nil {
		return err, nil
	}

	if operation == operator.Equal { //if operator does not exist return raw value
		return valFromReference, nil
	}

	foundValue, err = valFromReferenceToString(valFromReference, string(operation))
	if err != nil {
		return "", err
	}

	return string(operation) + foundValue.(string), nil
}

//Parse value to string
func valFromReferenceToString(value interface{}, operator string) (string, error) {

	switch typed := value.(type) {
	case string:
		return typed, nil
	case int, int64:
		return fmt.Sprintf("%d", value), nil
	case float64:
		return fmt.Sprintf("%f", value), nil
	default:
		return "", fmt.Errorf("Incorrect expression. Operator %s does not match with value: %v", operator, value)
	}
}

func FindAndShiftReferences(log logr.Logger, value, shift, pivot string) string {
	for _, reference := range regexReferences.FindAllString(value, -1) {

		index := strings.Index(reference, pivot)
		if index == -1 {
			log.Error(fmt.Errorf(`Failed to shit reference. Pivot value "%s" was not found`, pivot), "pivot search failed")
		}

		// try to get rule index from the reference
		if pivot == "anyPattern" {
			ruleIndex := strings.Split(reference[index+len(pivot)+1:], "/")[0]
			pivot = pivot + "/" + ruleIndex
		}

		shiftedReference := strings.Replace(reference, pivot, pivot+"/"+shift, 1)
		value = strings.Replace(value, reference, shiftedReference, -1)
	}

	return value
}

func formAbsolutePath(referencePath, absolutePath string) string {
	if path.IsAbs(referencePath) {
		return referencePath
	}

	return path.Join(absolutePath, referencePath)
}

func getValueFromReference(fullDocument interface{}, path string) (interface{}, error) {
	var element interface{}

	if _, err := jsonUtils.NewTraversal(fullDocument, jsonUtils.OnlyForLeafs(
		func(data *jsonUtils.ActionData) (interface{}, error) {
			if common.RemoveAnchorsFromPath(data.Path) == path {
				element = data.Element
			}

			return data.Element, nil
		})).TraverseJSON(); err != nil {
		return nil, err
	}

	return element, nil
}

func SubstituteAllInRule(log logr.Logger, ctx context.EvalInterface, typedRule kyverno.Rule) (_ kyverno.Rule, err error) {
	var rule interface{}

	rule, err = RuleToUntyped(typedRule)
	if err != nil {
		return typedRule, err
	}

	rule, err = substituteReferences(log, rule)
	if err != nil {
		return typedRule, err
	}

	rule, err = substituteVars(log, ctx, rule)
	if err != nil {
		return typedRule, err
	}

	return UntypedToRule(rule)
}

func RuleToUntyped(rule kyverno.Rule) (interface{}, error) {
	jsonRule, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}

	var untyped interface{}
	err = json.Unmarshal(jsonRule, &untyped)
	if err != nil {
		return nil, err
	}

	return untyped, nil
}

func UntypedToRule(untyped interface{}) (kyverno.Rule, error) {
	jsonRule, err := json.Marshal(untyped)
	if err != nil {
		return kyverno.Rule{}, err
	}

	var rule kyverno.Rule
	err = json.Unmarshal(jsonRule, &rule)
	if err != nil {
		return kyverno.Rule{}, err
	}

	return rule, nil
}

func replaceSubstituteVariables(document interface{}) interface{} {
	rawDocument, err := json.Marshal(document)
	if err != nil {
		return document
	}

	regex := regexp.MustCompile(`\{\{([^{}]*)\}\}`)
	for {
		if len(regex.FindAllStringSubmatch(string(rawDocument), -1)) > 0 {
			rawDocument = regex.ReplaceAll(rawDocument, []byte(`placeholderValue`))
		} else {
			break
		}
	}

	var output interface{}
	err = json.Unmarshal(rawDocument, &output)
	if err != nil {
		return document
	}

	return output
}
