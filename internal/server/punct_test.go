package server

import "testing"

func TestLocalPunctuate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"我们今天一起去看电影然后吃饭然后再回家明天早上还要开会讨论项目进度", "我们今天一起去看电影，然后吃饭，然后再回家明天早上还要开会讨论项目进度。"},
		{"你准备怎么办", "你准备怎么办？"},
		{"你去不去呢", "你去不去呢？"},
		{"今天天气很好", "今天天气很好。"},
		{"已经带标点了。", "已经带标点了。"},
		{"明天9点开会", "明天9点开会。"},
		{"因为下雨所以取消了", "因为下雨，所以取消了。"},
		{"我去买菜然后做饭最后洗碗", "我去买菜，然后做饭，最后洗碗。"},
		{"这个方案可以吗", "这个方案可以吗？"},
		{"我们要去上海出差然后回北京", "我们要去上海出差，然后回北京。"},
	}
	for _, c := range cases {
		if got := localPunctuate(c.in); got != c.want {
			t.Errorf("localPunctuate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitClauses(t *testing.T) {
	// 短文本低于 minClauseRunes 不切分。
	short := "我今天去超市买东西然后回家做饭"
	if parts := splitClauses(short); len(parts) != 1 {
		t.Fatalf("short splitClauses = %v, want 1 part", parts)
	}
	// 长文本按连接词切成多块，且所有块拼回等于原文。
	long := ""
	for i := 0; i < 12; i++ {
		long += "我今天去超市买东西然后回家做饭最后洗碗"
	}
	parts := splitClauses(long)
	if len(parts) < 3 {
		t.Fatalf("long splitClauses parts = %d, want >=3", len(parts))
	}
	joined := ""
	for _, p := range parts {
		if len([]rune(p)) < minClauseRunes {
			t.Fatalf("found undersized part len=%d: %q", len([]rune(p)), p)
		}
		joined += p
	}
	if joined != long {
		t.Fatalf("joined %q != input %q", joined, long)
	}
}
