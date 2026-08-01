package store

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"newmovie/internal/model"
)

// 回归：批量写入必须合并落盘，而不是每条都全量序列化整个库（O(n²)）。
// 旧实现下 3000 条目累计分配近 5GB，ARM 小内存设备会被 OOM Killer 杀掉 → 无限重启。
func TestFlushIsDebounced(t *testing.T) {
	dir := t.TempDir()
	st, err := NewJSONStore(dir + "/db.json")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	const n = 3000
	for i := 0; i < n; i++ {
		if e := st.SaveMediaItem(model.MediaItem{
			ID: "it-" + itoa(i), LibraryID: "lib1", Title: "标题" + itoa(i), Year: 2020,
		}); e != nil {
			t.Fatal(e)
		}
	}
	elapsed := time.Since(start)

	// 合并落盘后写 3000 条应在亚秒级；旧实现要 10s 以上。
	if elapsed > 5*time.Second {
		t.Fatalf("写入 %d 条耗时 %v，合并落盘疑似失效（旧的 O(n²) 行为）", n, elapsed)
	}

	// 关闭后数据必须完整落盘
	if e := st.Close(); e != nil {
		t.Fatal(e)
	}
	items, _ := st.ListMediaItems("lib1")
	if len(items) != n {
		t.Fatalf("期望 %d 条，实际 %d 条", n, len(items))
	}

	b, e := os.ReadFile(dir + "/db.json")
	if e != nil {
		t.Fatalf("Close 后应已落盘: %v", e)
	}
	var back map[string]interface{}
	if e := json.Unmarshal(b, &back); e != nil {
		t.Fatalf("落盘文件不是合法 JSON（原子写入失效）: %v", e)
	}
}

// 回归：Close 后重新打开，数据必须还在（防止丢最后一批写入）。
func TestClosePersistsData(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/db.json"

	st, _ := NewJSONStore(p)
	_ = st.SaveSetting("tmdb_api_key", "k-123")
	_ = st.SaveMediaItem(model.MediaItem{ID: "m1", LibraryID: "l1", Title: "测试影片"})
	if e := st.Close(); e != nil {
		t.Fatal(e)
	}

	st2, e := NewJSONStore(p)
	if e != nil {
		t.Fatal(e)
	}
	v, _ := st2.GetSetting("tmdb_api_key")
	if v != "k-123" {
		t.Fatalf("设置未持久化，得到 %q", v)
	}
	it, err := st2.GetMediaItem("m1")
	if err != nil || it.Title != "测试影片" {
		t.Fatalf("条目未持久化: %v %+v", err, it)
	}
}

// 落盘必须是原子的：不能出现半截 JSON（进程被杀时会写坏库，导致下次启动失败 → 无限重启）。
func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/db.json"
	st, _ := NewJSONStore(p)
	_ = st.SaveMediaItem(model.MediaItem{ID: "m1", LibraryID: "l1", Title: "原子写入"})
	_ = st.Close()

	// 不应残留临时文件
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatal("残留了 .tmp 临时文件，rename 未生效")
	}
	b, _ := os.ReadFile(p)
	var m map[string]interface{}
	if e := json.Unmarshal(b, &m); e != nil {
		t.Fatalf("落盘内容非法: %v", e)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
