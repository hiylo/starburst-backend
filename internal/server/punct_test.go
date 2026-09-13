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
	s := "我今天去超市买东西然后回家做饭最后洗碗然后再去看电影"
	parts := splitClauses(s)
	if len(parts) < 3 {
		t.Fatalf("splitClauses parts = %v, want >=3", parts)
	}
	joined := ""
	for _, p := range parts {
		joined += p
	}
	if joined != s {
		t.Fatalf("joined %q != input %q", joined, s)
	}
}
