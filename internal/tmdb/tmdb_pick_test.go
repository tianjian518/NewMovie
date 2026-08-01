package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPickBest_ChineseNearMiss 真实案例回归。
//
// 搜「将夜」(2026) 时 TMDB 返回的第一条是《昨夜将至》（只因共享「夜」「将」二字），
// 真正的《将夜》排第二。旧实现盲取 results[0]，给用户配上了完全不相干的海报。
func TestPickBest_ChineseNearMiss(t *testing.T) {
	rs := []resultItem{
		{ID: 291947, Name: "昨夜将至", OriginalName: "昨夜将至", FirstAirDate: "2026-06-24"},
		{ID: 282136, Name: "将夜", OriginalName: "将夜", FirstAirDate: "2026-04-23"},
	}
	if got := pickBest(rs, "将夜", 2026); got != 1 {
		t.Errorf("选中下标 = %d（%s），期望 1（将夜）", got, rs[maxInt(got, 0)].Name)
	}
}

// TestPickBest_ExactBeatsYear 标题完全一致应压过年份吻合的近似标题。
func TestPickBest_ExactBeatsYear(t *testing.T) {
	rs := []resultItem{
		{ID: 1, Name: "将夜外传", FirstAirDate: "2026-01-01"},
		{ID: 2, Name: "将夜", FirstAirDate: "2018-10-31"},
	}
	if got := pickBest(rs, "将夜", 2026); got != 1 {
		t.Errorf("选中下标 = %d，期望 1（标题完全一致优先）", got)
	}
}

// TestPickBest_YearBreaksTie 同名多季时用年份区分。
func TestPickBest_YearBreaksTie(t *testing.T) {
	rs := []resultItem{
		{ID: 83612, Name: "将夜", FirstAirDate: "2018-10-31"},
		{ID: 282136, Name: "将夜", FirstAirDate: "2026-04-23"},
	}
	if got := pickBest(rs, "将夜", 2026); got != 1 {
		t.Errorf("选中下标 = %d，期望 1（年份吻合）", got)
	}
	if got := pickBest(rs, "将夜", 2018); got != 0 {
		t.Errorf("选中下标 = %d，期望 0（年份吻合）", got)
	}
}

// TestPickBest_NoRelation 标题毫无关系时宁可不刮，也不要配错海报。
func TestPickBest_NoRelation(t *testing.T) {
	rs := []resultItem{
		{ID: 1, Name: "完全不相干的剧", FirstAirDate: "2026-01-01"},
	}
	if got := pickBest(rs, "将夜", 2026); got != -1 {
		t.Errorf("选中下标 = %d，期望 -1（不应强行匹配）", got)
	}
}

// TestPickBest_OriginalName 中文剧常只在 original_name 与查询一致。
func TestPickBest_OriginalName(t *testing.T) {
	rs := []resultItem{
		{ID: 1, Name: "Something Else", FirstAirDate: "2020-01-01"},
		{ID: 2, Name: "Ever Night", OriginalName: "将夜", FirstAirDate: "2018-10-31"},
	}
	if got := pickBest(rs, "将夜", 0); got != 1 {
		t.Errorf("选中下标 = %d，期望 1（匹配 original_name）", got)
	}
}

// TestPickBest_NormalizesPunctuation 标点/空格差异不应影响匹配。
func TestPickBest_NormalizesPunctuation(t *testing.T) {
	rs := []resultItem{{ID: 1, Name: "三体：黑暗森林"}}
	if got := pickBest(rs, "三体 黑暗森林", 0); got != 0 {
		t.Errorf("选中下标 = %d，期望 0（归一化后应匹配）", got)
	}
}

// TestSearch_RetriesWithoutYear 带年份搜不到时应去掉年份再搜一次。
// TMDB 对国产剧的年份过滤常年不准（首播年/上线年/目录年三者不一致），
// 直接返回空会让整部剧刮不到海报。
func TestSearch_RetriesWithoutYear(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("first_air_date_year") != "" || r.URL.Query().Get("year") != "" {
			_, _ = w.Write([]byte(`{"results":[]}`)) // 带年份搜不到
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":83612,"name":"将夜","first_air_date":"2018-10-31"}]}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client(), Lang: "zh-CN"}
	m, err := c.Search(context.Background(), "series", "将夜", 2026)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if m == nil {
		t.Fatal("去掉年份重搜后仍为 nil")
	}
	if m.TMDBID != 83612 {
		t.Errorf("tmdb id = %d，期望 83612", m.TMDBID)
	}
	if calls != 2 {
		t.Errorf("请求次数 = %d，期望 2（首次带年份 + 重试不带）", calls)
	}
}

// TestSearch_NoMatchReturnsNil 搜索有结果但都不相干时应返回 nil 而非乱配。
func TestSearch_NoMatchReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":9,"name":"毫不相干","first_air_date":"2020-01-01"}]}`))
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client(), Lang: "zh-CN"}
	m, err := c.Search(context.Background(), "series", "将夜", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if m != nil {
		t.Errorf("应返回 nil，实际配到了 %+v", m)
	}
}
