// Package contract validates endpoint response JSON against field contracts.
package contract

import "encoding/json"

// FieldSpec describes the contract of a single response field.
type FieldSpec struct {
	Name     string `json:"name"`     // field name
	Required bool   `json:"required"` // whether the field is required
	Type     string `json:"type"`     // expected type: string | number | boolean | object | array | null
}

// CheckResult reports the outcome for a single field.
type CheckResult struct {
	Field  string `json:"field"`
	Status string `json:"status"` // passed | missing | null | type_mismatch | unexpected
	Reason string `json:"reason"`
}

// CheckResponse validates responseJSON, a JSON object or array, against the
// given field specs.
//
// For an object response each field is checked individually. For an array
// response the first element is checked (list endpoints). The rules are:
//
//   - a missing field with Required=true yields missing; a missing optional
//     field yields passed
//   - a present field whose value is null and Required=true yields null
//   - a type mismatch, when Type is non-empty, yields type_mismatch
//   - a response whose root is neither an object nor an array of objects yields
//     unexpected for every field
//
// Results are returned in FieldSpec order and include passed fields so callers
// can compare the full set.
func CheckResponse(specs []FieldSpec, responseJSON []byte) ([]CheckResult, error) {
	var root any
	if err := json.Unmarshal(responseJSON, &root); err != nil {
		return nil, err
	}
	// GraphQL responses are wrapped in {"data": ...}; unwrap it when present so
	// the field specs apply to the payload rather than the envelope.
	if m, ok := root.(map[string]any); ok {
		if inner, has := m["data"]; has {
			root = inner
		}
	}

	var obj map[string]any
	unexpected := ""
	switch r := root.(type) {
	case map[string]any:
		obj = r
	case []any:
		if len(r) == 0 {
			obj = map[string]any{}
		} else if first, ok := r[0].(map[string]any); ok {
			obj = first
		} else {
			unexpected = typeName(r[0])
		}
	default:
		unexpected = typeName(root)
	}

	results := make([]CheckResult, 0, len(specs))
	if unexpected != "" {
		for _, s := range specs {
			results = append(results, CheckResult{
				Field:  s.Name,
				Status: "unexpected",
				Reason: "unexpected response shape: " + unexpected,
			})
		}
		return results, nil
	}

	for _, s := range specs {
		results = append(results, checkField(s, obj))
	}
	return results, nil
}

// CheckEndpoints validates multiple endpoints in one call. It maps each
// endpoint ID to the field-check results produced by CheckResponse. Endpoints
// missing from responses are skipped; an endpoint whose JSON fails to parse is
// reported as a single unexpected result.
func CheckEndpoints(specs map[string][]FieldSpec, responses map[string][]byte) map[string][]CheckResult {
	out := make(map[string][]CheckResult, len(specs))
	for id, spec := range specs {
		resp, ok := responses[id]
		if !ok {
			continue
		}
		results, err := CheckResponse(spec, resp)
		if err != nil {
			results = []CheckResult{{Status: "unexpected", Reason: err.Error()}}
		}
		out[id] = results
	}
	return out
}

// checkField checks a single spec against the decoded object.
func checkField(s FieldSpec, obj map[string]any) CheckResult {
	res := CheckResult{Field: s.Name}
	v, ok := obj[s.Name]
	if !ok {
		if s.Required {
			res.Status = "missing"
			res.Reason = "required field is missing"
		} else {
			res.Status = "passed"
			res.Reason = "optional field is absent"
		}
		return res
	}
	if v == nil {
		if s.Required {
			res.Status = "null"
			res.Reason = "required field must not be null"
		} else {
			res.Status = "passed"
			res.Reason = "optional field is null"
		}
		return res
	}
	if s.Type != "" && typeName(v) != s.Type {
		res.Status = "type_mismatch"
		res.Reason = "expected " + s.Type + ", got " + typeName(v)
		return res
	}
	res.Status = "passed"
	res.Reason = "field matches contract"
	return res
}

// typeName maps a decoded JSON value to one of the canonical type names:
// string, number, boolean, object, array or null. Integer and float variants
// are all reported as number.
func typeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}
