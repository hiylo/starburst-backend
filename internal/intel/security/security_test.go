package security

import (
	"reflect"
	"testing"
)

func TestDetectField(t *testing.T) {
	tests := []struct {
		name  string
		field Field
		want  *Warning
	}{
		{
			name:  "password name",
			field: Field{Name: "password"},
			want:  &Warning{Field: "password", Kind: "password", Severity: SeverityCritical, Message: messages["password"]},
		},
		{
			name:  "passwd name",
			field: Field{Name: "passwd"},
			want:  &Warning{Field: "passwd", Kind: "password", Severity: SeverityCritical, Message: messages["password"]},
		},
		{
			name:  "pwd name",
			field: Field{Name: "pwd"},
			want:  &Warning{Field: "pwd", Kind: "password", Severity: SeverityCritical, Message: messages["password"]},
		},
		{
			name:  "password embedded in camelCase",
			field: Field{Name: "userPassword"},
			want:  &Warning{Field: "userPassword", Kind: "password", Severity: SeverityCritical, Message: messages["password"]},
		},
		{
			name:  "secret name",
			field: Field{Name: "secret"},
			want:  &Warning{Field: "secret", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "token name",
			field: Field{Name: "token"},
			want:  &Warning{Field: "token", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "apiKey camelCase",
			field: Field{Name: "apiKey"},
			want:  &Warning{Field: "apiKey", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "apiKey snake_case",
			field: Field{Name: "api_key"},
			want:  &Warning{Field: "api_key", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "accessKey name",
			field: Field{Name: "accessKey"},
			want:  &Warning{Field: "accessKey", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "privateKey name",
			field: Field{Name: "privateKey"},
			want:  &Warning{Field: "privateKey", Kind: "secret", Severity: SeverityCritical, Message: messages["secret"]},
		},
		{
			name:  "idCard camelCase",
			field: Field{Name: "idCard"},
			want:  &Warning{Field: "idCard", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "idcard lowercase",
			field: Field{Name: "idcard"},
			want:  &Warning{Field: "idcard", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "identityCard name",
			field: Field{Name: "identityCard"},
			want:  &Warning{Field: "identityCard", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "idcard snake_case",
			field: Field{Name: "id_card"},
			want:  &Warning{Field: "id_card", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "idcard uppercase",
			field: Field{Name: "IDCARD"},
			want:  &Warning{Field: "IDCARD", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "idcard chinese",
			field: Field{Name: "身份证"},
			want:  &Warning{Field: "身份证", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		},
		{
			name:  "bankCard camelCase",
			field: Field{Name: "bankCard"},
			want:  &Warning{Field: "bankCard", Kind: "bankcard", Severity: SeverityHigh, Message: messages["bankcard"]},
		},
		{
			name:  "bankcard lowercase",
			field: Field{Name: "bankcard"},
			want:  &Warning{Field: "bankcard", Kind: "bankcard", Severity: SeverityHigh, Message: messages["bankcard"]},
		},
		{
			name:  "bankAccount name",
			field: Field{Name: "bankAccount"},
			want:  &Warning{Field: "bankAccount", Kind: "bankcard", Severity: SeverityHigh, Message: messages["bankcard"]},
		},
		{
			name:  "bankcard chinese",
			field: Field{Name: "银行卡"},
			want:  &Warning{Field: "银行卡", Kind: "bankcard", Severity: SeverityHigh, Message: messages["bankcard"]},
		},
		{
			name:  "mobile name",
			field: Field{Name: "mobile"},
			want:  &Warning{Field: "mobile", Kind: "mobile", Severity: SeverityMedium, Message: messages["mobile"]},
		},
		{
			name:  "phone name",
			field: Field{Name: "phone"},
			want:  &Warning{Field: "phone", Kind: "mobile", Severity: SeverityMedium, Message: messages["mobile"]},
		},
		{
			name:  "phoneNumber name",
			field: Field{Name: "phoneNumber"},
			want:  &Warning{Field: "phoneNumber", Kind: "mobile", Severity: SeverityMedium, Message: messages["mobile"]},
		},
		{
			name:  "mobile chinese",
			field: Field{Name: "手机号"},
			want:  &Warning{Field: "手机号", Kind: "mobile", Severity: SeverityMedium, Message: messages["mobile"]},
		},
		{
			name:  "email name",
			field: Field{Name: "email"},
			want:  &Warning{Field: "email", Kind: "email", Severity: SeverityLow, Message: messages["email"]},
		},
		{
			name:  "mail name",
			field: Field{Name: "mail"},
			want:  &Warning{Field: "mail", Kind: "email", Severity: SeverityLow, Message: messages["email"]},
		},
		{
			name:  "amount with BigDecimal type",
			field: Field{Name: "amount", Type: "BigDecimal"},
			want:  &Warning{Field: "amount", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
		},
		{
			name:  "balance with Float type",
			field: Field{Name: "balance", Type: "Float"},
			want:  &Warning{Field: "balance", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
		},
		{
			name:  "money with Double type",
			field: Field{Name: "money", Type: "Double"},
			want:  &Warning{Field: "money", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
		},
		{
			name:  "price with fully qualified type",
			field: Field{Name: "price", Type: "java.math.BigDecimal"},
			want:  &Warning{Field: "price", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
		},
		{
			name:  "amount chinese",
			field: Field{Name: "金额", Type: "BigDecimal"},
			want:  &Warning{Field: "金额", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
		},
		{
			name:  "amount with non numeric type not sensitive",
			field: Field{Name: "amount", Type: "String"},
			want:  nil,
		},
		{
			name:  "amount with empty type not sensitive",
			field: Field{Name: "amount"},
			want:  nil,
		},
		{
			name:  "ordinary name field",
			field: Field{Name: "name"},
			want:  nil,
		},
		{
			name:  "ordinary title field",
			field: Field{Name: "title"},
			want:  nil,
		},
		{
			name:  "ordinary userName field",
			field: Field{Name: "userName"},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectField(tt.field)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DetectField(%#v) = %#v, want %#v", tt.field, got, tt.want)
			}
		})
	}
}

func TestDetectFields(t *testing.T) {
	fields := []Field{
		{Name: "name"},
		{Name: "userPassword"},
		{Name: "id_card"},
		{Name: "title"},
		{Name: "mobile", Type: "String"},
		{Name: "amount", Type: "String"},
		{Name: "balance", Type: "BigDecimal"},
	}
	want := []Warning{
		{Field: "userPassword", Kind: "password", Severity: SeverityCritical, Message: messages["password"]},
		{Field: "id_card", Kind: "idcard", Severity: SeverityHigh, Message: messages["idcard"]},
		{Field: "mobile", Kind: "mobile", Severity: SeverityMedium, Message: messages["mobile"]},
		{Field: "balance", Kind: "amount", Severity: SeverityMedium, Message: messages["amount"]},
	}
	got := DetectFields(fields)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DetectFields() = %#v, want %#v", got, want)
	}

	if got := DetectFields(nil); got == nil || len(got) != 0 {
		t.Fatalf("DetectFields(nil) = %#v, want empty slice", got)
	}
}

func TestClassifyIssue(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		severity   string
		summary    string
		fieldNames []string
		wantKind   string
		wantSev    string
	}{
		{
			name:       "password field escalates to critical security warning",
			kind:       "bug",
			severity:   "low",
			summary:    "endpoint returns user profile",
			fieldNames: []string{"password"},
			wantKind:   "security_warning",
			wantSev:    "critical",
		},
		{
			name:       "secret field escalates to critical security warning",
			kind:       "bug",
			severity:   "medium",
			summary:    "",
			fieldNames: []string{"accessKey"},
			wantKind:   "security_warning",
			wantSev:    "critical",
		},
		{
			name:       "idcard field escalates to high security warning",
			kind:       "bug",
			severity:   "low",
			summary:    "",
			fieldNames: []string{"id_card"},
			wantKind:   "security_warning",
			wantSev:    "high",
		},
		{
			name:       "summary sensitive keyword escalates",
			kind:       "bug",
			severity:   "low",
			summary:    "接口返回用户手机号未脱敏",
			fieldNames: nil,
			wantKind:   "security_warning",
			wantSev:    "medium",
		},
		{
			name:       "original higher severity is preserved",
			kind:       "bug",
			severity:   "critical",
			summary:    "",
			fieldNames: []string{"mail"},
			wantKind:   "security_warning",
			wantSev:    "critical",
		},
		{
			name:       "no sensitive match returns original unchanged",
			kind:       "bug",
			severity:   "low",
			summary:    "user profile update fails",
			fieldNames: []string{"name", "title"},
			wantKind:   "bug",
			wantSev:    "low",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotSev := ClassifyIssue(tt.kind, tt.severity, tt.summary, tt.fieldNames)
			if gotKind != tt.wantKind || gotSev != tt.wantSev {
				t.Fatalf("ClassifyIssue(%q, %q, %q, %#v) = (%q, %q), want (%q, %q)",
					tt.kind, tt.severity, tt.summary, tt.fieldNames, gotKind, gotSev, tt.wantKind, tt.wantSev)
			}
		})
	}
}
