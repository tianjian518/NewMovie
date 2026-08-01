package openlist

import (
	"encoding/json"
	"testing"
	"time"
)

func mustUnix(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

// 模拟 OpenList 系接口把 modified/size 混用数字与字符串（含 ISO 日期）的真实返回，
// 验证 FsObj 解析不再因类型不一致而整体失败。
func TestFsObj_FlexInt64FromOpenList(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSz  int64
		wantMod int64
	}{
		{
			name:    "全数字（标准 Alist）",
			body:    `{"name":"a.mp4","size":1234,"is_dir":false,"modified":1699000000}`,
			wantSz:  1234,
			wantMod: 1699000000,
		},
		{
			// 触发 1.1.x「测试连接」报错的原始场景
			name:    "modified 为字符串",
			body:    `{"name":"a.mp4","size":1234,"is_dir":false,"modified":"1699000000"}`,
			wantSz:  1234,
			wantMod: 1699000000,
		},
		{
			name:    "size 与 modified 均为字符串",
			body:    `{"name":"a.mp4","size":"5678","is_dir":false,"modified":"1700000000"}`,
			wantSz:  5678,
			wantMod: 1700000000,
		},
		{
			name:    "浮点时间戳",
			body:    `{"name":"a.mp4","size":1234,"is_dir":false,"modified":1699000000.0}`,
			wantSz:  1234,
			wantMod: 1699000000,
		},
		{
			name:    "ISO 日期串",
			body:    `{"name":"a.mp4","size":1234,"is_dir":false,"modified":"2023-11-03T07:46:40Z"}`,
			wantSz:  1234,
			wantMod: mustUnix("2023-11-03T07:46:40Z"),
		},
		{
			// 复现真实 OpenList（139cas）返回：纳秒精度 RFC3339 串
			name:    "ISO 纳秒串（真实 OpenList）",
			body:    `{"name":"a.mp4","size":1234,"is_dir":false,"modified":"2026-07-29T14:12:07.778495506Z"}`,
			wantSz:  1234,
			wantMod: mustUnix("2026-07-29T14:12:07.778495506Z"),
		},
		{
			name:    "空/缺省回退 0",
			body:    `{"name":"a.mp4"}`,
			wantSz:  0,
			wantMod: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var o FsObj
			if err := json.Unmarshal([]byte(c.body), &o); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if int64(o.Size) != c.wantSz {
				t.Errorf("size = %d, want %d", int64(o.Size), c.wantSz)
			}
			if int64(o.Modified) != c.wantMod {
				t.Errorf("modified = %d, want %d", int64(o.Modified), c.wantMod)
			}
		})
	}
}

// 整段列表响应里只要有一个对象字段串化，也不应让整次解析失败（测试连接能返回网盘列表）。
func TestFsListResp_FlexInt64DoesNotAbortWholeList(t *testing.T) {
	body := `{
		"code": 200,
		"message": "success",
		"data": {
			"content": [
				{"name":"drive1","size":"0","is_dir":true,"modified":"1699000000"},
				{"name":"movie.mp4","size":1234,"is_dir":false,"modified":1699000100}
			],
			"total": 2
		}
	}`
	var r fsListResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("整段列表解析失败: %v", err)
	}
	if len(r.Data.Content) != 2 {
		t.Fatalf("期望 2 个对象，实际 %d", len(r.Data.Content))
	}
	if int64(r.Data.Content[0].Modified) != 1699000000 {
		t.Errorf("drive1.modified = %d, want 1699000000", int64(r.Data.Content[0].Modified))
	}
}

// 真实 139cas OpenList 把 type 返回成数字，FsObj.Type 必须能接受。
func TestFsObj_TypeAsNumber(t *testing.T) {
	body := `{"name":"movie","size":1234,"is_dir":false,"modified":"2026-07-29T14:12:07.778495506Z","type":1}`
	var o FsObj
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if string(o.Type) != "1" {
		t.Errorf("type = %q, want \"1\"", string(o.Type))
	}
	if int64(o.Modified) != mustUnix("2026-07-29T14:12:07.778495506Z") {
		t.Errorf("modified = %d, want %d", int64(o.Modified), mustUnix("2026-07-29T14:12:07.778495506Z"))
	}
}

// 用真实 OpenList 的字段形态（type=int, modified=纳秒串, size=int）跑整段列表，确保不崩。
func TestFsListResp_RealOpenListShape(t *testing.T) {
	body := `{
		"code": 200, "message": "success",
		"data": {
			"content": [
				{"name":"115","size":0,"is_dir":true,"modified":"2026-07-29T14:12:07.778495506Z","type":1},
				{"name":"movie.mp4","size":1073741824,"is_dir":false,"modified":"2026-07-30T00:13:21.496688814Z","type":1}
			],
			"total": 2
		}
	}`
	var r fsListResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("整段列表解析失败: %v", err)
	}
	if len(r.Data.Content) != 2 {
		t.Fatalf("期望 2 个对象，实际 %d", len(r.Data.Content))
	}
	if string(r.Data.Content[0].Type) != "1" {
		t.Errorf("drive.type = %q, want \"1\"", string(r.Data.Content[0].Type))
	}
	if int64(r.Data.Content[1].Size) != 1073741824 {
		t.Errorf("size = %d, want 1073741824", int64(r.Data.Content[1].Size))
	}
}
