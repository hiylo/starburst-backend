package contract

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCheckResponse(t *testing.T) {
	tests := []struct {
		name    string
		specs   []FieldSpec
		resp    string
		want    []CheckResult
		wantErr bool
	}{
		{
			name: "all field statuses on object",
			specs: []FieldSpec{
				{Name: "id", Required: true, Type: "number"},
				{Name: "name", Required: true, Type: "string"},
				{Name: "active", Required: false, Type: "boolean"},
				{Name: "tags", Required: false, Type: "array"},
				{Name: "meta", Required: false, Type: "object"},
				{Name: "nickname", Required: true, Type: "string"},
				{Name: "deleted", Required: true, Type: "boolean"},
				{Name: "email", Required: true, Type: "string"},
			},
			resp: `{"id":1,"name":"banner","active":true,"tags":["a"],"meta":{"k":"v"},"nickname":null,"deleted":"yes"}`,
			want: []CheckResult{
				{Field: "id", Status: "passed", Reason: "field matches contract"},
				{Field: "name", Status: "passed", Reason: "field matches contract"},
				{Field: "active", Status: "passed", Reason: "field matches contract"},
				{Field: "tags", Status: "passed", Reason: "field matches contract"},
				{Field: "meta", Status: "passed", Reason: "field matches contract"},
				{Field: "nickname", Status: "null", Reason: "required field must not be null"},
				{Field: "deleted", Status: "type_mismatch", Reason: "expected boolean, got string"},
				{Field: "email", Status: "missing", Reason: "required field is missing"},
			},
		},
		{
			name:  "optional field missing passes",
			specs: []FieldSpec{{Name: "bio", Required: false, Type: "string"}},
			resp:  `{}`,
			want: []CheckResult{
				{Field: "bio", Status: "passed", Reason: "optional field is absent"},
			},
		},
		{
			name:  "empty object with required field is missing",
			specs: []FieldSpec{{Name: "id", Required: true, Type: "number"}},
			resp:  `{}`,
			want: []CheckResult{
				{Field: "id", Status: "missing", Reason: "required field is missing"},
			},
		},
		{
			name:  "optional null passes",
			specs: []FieldSpec{{Name: "bio", Required: false, Type: "string"}},
			resp:  `{"bio":null}`,
			want: []CheckResult{
				{Field: "bio", Status: "passed", Reason: "optional field is null"},
			},
		},
		{
			name:  "array response uses first element",
			specs: []FieldSpec{{Name: "id", Required: true, Type: "number"}},
			resp:  `[{"id":"x"},{"id":2}]`,
			want: []CheckResult{
				{Field: "id", Status: "type_mismatch", Reason: "expected number, got string"},
			},
		},
		{
			name: "empty array marks required fields missing",
			specs: []FieldSpec{
				{Name: "id", Required: true, Type: "number"},
				{Name: "name", Required: true, Type: "string"},
			},
			resp: `[]`,
			want: []CheckResult{
				{Field: "id", Status: "missing", Reason: "required field is missing"},
				{Field: "name", Status: "missing", Reason: "required field is missing"},
			},
		},
		{
			name:    "invalid JSON returns error",
			specs:   []FieldSpec{{Name: "id", Required: true, Type: "number"}},
			resp:    `{"id":`,
			wantErr: true,
		},
		{
			name:  "scalar root is unexpected",
			specs: []FieldSpec{{Name: "id", Required: true, Type: "number"}},
			resp:  `"just a string"`,
			want: []CheckResult{
				{Field: "id", Status: "unexpected", Reason: "unexpected response shape: string"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckResponse(tt.specs, []byte(tt.resp))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CheckResponse() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckResponse() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("CheckResponse() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTypeName(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{name: "string", v: "x", want: "string"},
		{name: "float64 number", v: 1.5, want: "number"},
		{name: "int number", v: 3, want: "number"},
		{name: "json number", v: json.Number("9"), want: "number"},
		{name: "bool", v: true, want: "boolean"},
		{name: "object", v: map[string]any{}, want: "object"},
		{name: "array", v: []any{}, want: "array"},
		{name: "null", v: nil, want: "null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := typeName(tt.v); got != tt.want {
				t.Fatalf("typeName(%#v) = %q, want %q", tt.v, got, tt.want)
			}
		})
	}
}

func TestCheckEndpoints(t *testing.T) {
	specs := map[string][]FieldSpec{
		"list": {
			{Name: "id", Required: true, Type: "number"},
			{Name: "name", Required: false, Type: "string"},
		},
		"detail": {
			{Name: "id", Required: true, Type: "number"},
		},
		"broken": {
			{Name: "id", Required: true, Type: "number"},
		},
		"absent": {
			{Name: "id", Required: true, Type: "number"},
		},
	}
	responses := map[string][]byte{
		"list":   []byte(`{"id":1}`),
		"detail": []byte(`{"id":"x"}`),
		"broken": []byte(`{"id":`),
	}

	got := CheckEndpoints(specs, responses)

	wantList := []CheckResult{
		{Field: "id", Status: "passed", Reason: "field matches contract"},
		{Field: "name", Status: "passed", Reason: "optional field is absent"},
	}
	if !reflect.DeepEqual(got["list"], wantList) {
		t.Fatalf("CheckEndpoints() list = %#v, want %#v", got["list"], wantList)
	}

	wantDetail := []CheckResult{
		{Field: "id", Status: "type_mismatch", Reason: "expected number, got string"},
	}
	if !reflect.DeepEqual(got["detail"], wantDetail) {
		t.Fatalf("CheckEndpoints() detail = %#v, want %#v", got["detail"], wantDetail)
	}

	if broken := got["broken"]; len(broken) != 1 || broken[0].Status != "unexpected" {
		t.Fatalf("CheckEndpoints() broken = %#v, want single unexpected result", broken)
	}

	if _, ok := got["absent"]; ok {
		t.Fatalf("CheckEndpoints() should skip endpoints with no response, got %#v", got["absent"])
	}
}

func TestCheckResponseGraphQLEnvelope(t *testing.T) {
	specs := []FieldSpec{
		{Name: "id", Required: true, Type: "number"},
		{Name: "title", Required: true, Type: "string"},
	}
	// A GraphQL-style {"data": {...}} payload must validate against the specs.
	results, err := CheckResponse(specs, []byte(`{"data":{"id":1,"title":"hello"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != "passed" {
			t.Errorf("envelope field %s = %q, want passed", r.Field, r.Status)
		}
	}
	// Without the envelope the same payload would report missing.
	results, err = CheckResponse(specs, []byte(`{"id":1,"title":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != "passed" {
			t.Errorf("plain field %s = %q, want passed", r.Field, r.Status)
		}
	}
	// Non-GraphQL object with its own "data" field that is NOT the payload still
	// unwraps; this matches the BFF convention where data is the payload.
	results, err = CheckResponse(specs, []byte(`{"data":{"id":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if results[1].Status != "missing" {
		t.Errorf("title under data envelope = %q, want missing", results[1].Status)
	}
}
