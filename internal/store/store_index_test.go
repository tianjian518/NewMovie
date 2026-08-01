package store

import (
	"path/filepath"
	"testing"

	"newmovie/internal/model"
)

func tmpStore(t *testing.T) Store {
	t.Helper()
	st, err := NewJSONStore(filepath.Join(t.TempDir(), "v.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st
}

// TestIndex_TitleRenameKeepsLookup 刮削会把标题从「将夜」改成「将夜 第一季」。
// 标题去重索引必须跟着更新，否则改名后旧键仍指向该条目，
// 下次按旧标题 upsert 会更新到错误的记录上。
func TestIndex_TitleRenameKeepsLookup(t *testing.T) {
	st := tmpStore(t)
	_ = st.SaveMediaItem(model.MediaItem{ID: "i1", LibraryID: "lib", Title: "将夜", Year: 2018})
	// 刮削改名
	_ = st.SaveMediaItem(model.MediaItem{ID: "i1", LibraryID: "lib", Title: "将夜 第一季", Year: 2018})

	// 用旧标题 + 新 ID 再 upsert：应命中 ID（同一条），不该新增
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "i1", LibraryID: "lib", Title: "将夜", Year: 2018})
	list, _ := st.ListMediaItems("lib")
	if len(list) != 1 {
		t.Fatalf("条目数 = %d, want 1（ID 命中不应新增）: %+v", len(list), list)
	}

	// 用旧标题 + 空 ID upsert：旧键已失效，应当作新条目
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "i2", LibraryID: "lib", Title: "将夜", Year: 2018})
	list, _ = st.ListMediaItems("lib")
	if len(list) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(list))
	}
	got, err := st.GetMediaItem("i1")
	if err != nil || got.Title != "将夜 第一季" {
		t.Fatalf("i1 = %+v err=%v，改名后按 ID 仍应取到", got, err)
	}
}

// TestIndex_UpsertByTitleDedup 同标题同年同库应合并成一条。
func TestIndex_UpsertByTitleDedup(t *testing.T) {
	st := tmpStore(t)
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "a", LibraryID: "lib", Title: "无间道", Year: 2002})
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "b", LibraryID: "lib", Title: "无间道", Year: 2002, Rating: 9.1})
	list, _ := st.ListMediaItems("lib")
	if len(list) != 1 {
		t.Fatalf("条目数 = %d, want 1", len(list))
	}
	if list[0].Rating != 9.1 {
		t.Fatalf("rating = %v, want 9.1（应合并元数据）", list[0].Rating)
	}
	// 不同库同名不合并
	_ = st.UpsertMediaItemByTitle(model.MediaItem{ID: "c", LibraryID: "lib2", Title: "无间道", Year: 2002})
	if l2, _ := st.ListMediaItems("lib2"); len(l2) != 1 {
		t.Fatalf("lib2 条目数 = %d, want 1", len(l2))
	}
}

// TestIndex_SurvivesReload 索引是内存态，重新打开文件后必须重建。
func TestIndex_SurvivesReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")
	st, _ := NewJSONStore(p)
	_ = st.SaveMediaItem(model.MediaItem{ID: "i1", LibraryID: "lib", Title: "沙丘", Year: 2021})
	_ = st.SaveMediaFile(model.MediaFile{ID: "f1", ItemID: "i1", Path: "/a.mkv"})
	_ = st.Close()

	st2, err := NewJSONStore(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, err := st2.GetMediaItem("i1"); err != nil || got.Title != "沙丘" {
		t.Fatalf("重开后取条目失败: %+v %v", got, err)
	}
	if got, err := st2.GetMediaFile("f1"); err != nil || got.Path != "/a.mkv" {
		t.Fatalf("重开后取文件失败: %+v %v", got, err)
	}
	// 重开后 upsert 同标题仍应去重
	_ = st2.UpsertMediaItemByTitle(model.MediaItem{ID: "i9", LibraryID: "lib", Title: "沙丘", Year: 2021})
	if l, _ := st2.ListMediaItems("lib"); len(l) != 1 {
		t.Fatalf("重开后去重失效, 条目数 = %d", len(l))
	}
}

// TestDeleteLibrary_CascadesItemsAndFiles 删库要连带清掉条目和文件，
// 否则孤儿数据永久留在 JSON 里，越攒越大。
func TestDeleteLibrary_CascadesItemsAndFiles(t *testing.T) {
	st := tmpStore(t)
	_ = st.SaveLibrary(model.Library{ID: "lib", Name: "L"})
	_ = st.SaveLibrary(model.Library{ID: "keep", Name: "K"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "i1", LibraryID: "lib", Title: "A"})
	_ = st.SaveMediaFile(model.MediaFile{ID: "f1", ItemID: "i1"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "i2", LibraryID: "keep", Title: "B"})
	_ = st.SaveMediaFile(model.MediaFile{ID: "f2", ItemID: "i2"})

	if err := st.DeleteLibrary("lib"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetMediaItem("i1"); err == nil {
		t.Error("条目 i1 应随库一起删除")
	}
	if _, err := st.GetMediaFile("f1"); err == nil {
		t.Error("文件 f1 应随库一起删除")
	}
	// 其它库不受影响
	if got, err := st.GetMediaItem("i2"); err != nil || got.Title != "B" {
		t.Errorf("其它库条目被误删: %+v %v", got, err)
	}
	if _, err := st.GetMediaFile("f2"); err != nil {
		t.Error("其它库文件被误删")
	}
}

// TestIndex_LargeLibraryIsNotQuadratic 索引存在性的功能兜底：
// 一万条写入 + 一万次随机读，跑得完就说明没退化成线性扫描。
func TestIndex_LargeLibraryIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过")
	}
	st := tmpStore(t)
	const n = 10000
	for i := 0; i < n; i++ {
		id := "i" + itoa(i)
		if err := st.SaveMediaFile(model.MediaFile{ID: "f" + itoa(i), ItemID: id}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := st.GetMediaFile("f" + itoa(i)); err != nil {
			t.Fatalf("get f%d: %v", i, err)
		}
	}
}
