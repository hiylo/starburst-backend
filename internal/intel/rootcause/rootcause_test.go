package rootcause

import "testing"

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name    string
		failure string
		hints   []string
		wantCat Category
		wantCon string
	}{
		{
			name:    "database SQLException",
			failure: "java.sql.SQLException: ORA-00942 table or view does not exist",
			wantCat: CategoryDatabase,
			wantCon: confidenceHigh,
		},
		{
			name:    "database deadlock",
			failure: "Transaction deadlock detected",
			wantCat: CategoryDatabase,
			wantCon: confidenceHigh,
		},
		{
			name:    "database constraint",
			failure: "SQLIntegrityConstraintViolationException: duplicate entry",
			wantCat: CategoryDatabase,
			wantCon: confidenceHigh,
		},
		{
			name:    "database priority over env",
			failure: "Connection refused to mysql at 127.0.0.1:3306",
			wantCat: CategoryDatabase,
			wantCon: confidenceMedium,
		},
		{
			name:    "database refused postgres",
			failure: "connection refused to postgres at localhost",
			wantCat: CategoryDatabase,
			wantCon: confidenceMedium,
		},
		{
			name:    "env command not found",
			failure: "bash: gradle: command not found",
			wantCat: CategoryEnv,
			wantCon: confidenceHigh,
		},
		{
			name:    "env no such file",
			failure: "java.io.IOException: No such file or directory",
			wantCat: CategoryEnv,
			wantCon: confidenceHigh,
		},
		{
			name:    "env unsupported platform",
			failure: "unsupported platform: linux/arm64",
			wantCat: CategoryEnv,
			wantCon: confidenceHigh,
		},
		{
			name:    "env non-database connection refused",
			failure: "dial tcp 127.0.0.1:8080: connection refused",
			wantCat: CategoryEnv,
			wantCon: confidenceHigh,
		},
		{
			name:    "contract missing field",
			failure: "response missing field 'bannerId'",
			wantCat: CategoryContract,
			wantCon: confidenceHigh,
		},
		{
			name:    "contract type mismatch",
			failure: "type mismatch: expected Integer but got String",
			wantCat: CategoryContract,
			wantCon: confidenceHigh,
		},
		{
			name:    "contract class cast",
			failure: "java.lang.ClassCastException: cannot cast",
			wantCat: CategoryContract,
			wantCon: confidenceHigh,
		},
		{
			name:    "contract expected but was",
			failure: "expected:<3> but was:<2>",
			wantCat: CategoryContract,
			wantCon: confidenceMedium,
		},
		{
			name:    "contract null keyword",
			failure: "field 'title' is null",
			wantCat: CategoryContract,
			wantCon: confidenceHigh,
		},
		{
			name:    "contract hint missing",
			failure: "banner_title is missing",
			hints:   []string{"banner_title"},
			wantCat: CategoryContract,
			wantCon: confidenceMedium,
		},
		{
			name:    "client NPE with android hint",
			failure: "java.lang.NullPointerException at HomeFragment",
			hints:   []string{"android_banner_image"},
			wantCat: CategoryClient,
			wantCon: confidenceMedium,
		},
		{
			name:    "client binding with layout hint",
			failure: "view binding failed to resolve layout",
			hints:   []string{"layout_banner"},
			wantCat: CategoryClient,
			wantCon: confidenceMedium,
		},
		{
			name:    "client missing field marker",
			failure: "CLIENT_MISSING_FIELD title not rendered",
			hints:   []string{"ui_title"},
			wantCat: CategoryClient,
			wantCon: confidenceMedium,
		},
		{
			name:    "backend null pointer without UI hint",
			failure: "java.lang.NullPointerException at com.example.Service",
			wantCat: CategoryBackend,
			wantCon: confidenceHigh,
		},
		{
			name:    "backend illegal argument",
			failure: "java.lang.IllegalArgumentException: bad value",
			wantCat: CategoryBackend,
			wantCon: confidenceHigh,
		},
		{
			name:    "backend out of bounds",
			failure: "java.lang.ArrayIndexOutOfBoundsException: index 5",
			wantCat: CategoryBackend,
			wantCon: confidenceHigh,
		},
		{
			name:    "backend assert failed",
			failure: "assertTrue failed: condition not met",
			wantCat: CategoryBackend,
			wantCon: confidenceMedium,
		},
		{
			name:    "backend expected less",
			failure: "expected:<3> to be less than:<2>",
			wantCat: CategoryBackend,
			wantCon: confidenceMedium,
		},
		{
			name:    "unknown fallback",
			failure: "lorem ipsum dolor sit amet",
			wantCat: CategoryUnknown,
			wantCon: confidenceLow,
		},
		{
			name:    "unknown empty",
			failure: "",
			wantCat: CategoryUnknown,
			wantCon: confidenceLow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.failure, tc.hints)
			if got.Category != tc.wantCat {
				t.Fatalf("Analyze() category = %q, want %q", got.Category, tc.wantCat)
			}
			if got.Confidence != tc.wantCon {
				t.Fatalf("Analyze() confidence = %q, want %q", got.Confidence, tc.wantCon)
			}
		})
	}
}

func TestAnalyzeMany(t *testing.T) {
	failures := []string{
		"java.sql.SQLException: connection lost",
		"bash: gradle: command not found",
		"java.lang.NullPointerException at HomeFragment",
		"unrelated noise",
	}
	hints := []string{"android_banner_image"}
	want := []Category{CategoryDatabase, CategoryEnv, CategoryClient, CategoryUnknown}

	got := AnalyzeMany(failures, hints)
	if len(got) != len(failures) {
		t.Fatalf("AnalyzeMany() returned %d results, want %d", len(got), len(failures))
	}
	for i := range got {
		if got[i].Category != want[i] {
			t.Fatalf("AnalyzeMany()[%d] category = %q, want %q", i, got[i].Category, want[i])
		}
	}
}
