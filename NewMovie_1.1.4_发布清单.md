# NewMovie 1.1.4 发布清单 — 修复 OpenList「测试连接」报错（modified 字段类型不匹配）

> 修复类型：功能性 Bug。配置了 OpenList / AList（尤其是 139cas 等 rebrand）存储源的用户都需要升级。

---

## 一、问题现象

在「设置 → 添加 OpenList 存储源」填好 Base URL / Token / 签名密钥后，点**测试连接**报错：

```
失败：{"error":"连接失败: json: cannot unmarshal string into Go struct field FsObj.data.content.modified of type int64"}
```

连通性其实是好的，只是响应解析阶段就崩了，连已挂载网盘列表都拿不到。

## 二、根因

`internal/openlist/client.go` 里 `FsObj` 把 `modified`（以及 `size`）定义成 `int64`：

```go
type FsObj struct {
    Size     int64  `json:"size"`
    Modified int64  `json:"modified"`
    ...
}
```

但 OpenList 系接口（尤其部分版本 / 139cas rebrand）实际把这两个字段**序列化成了字符串**：

```json
{ "name":"movie.mp4", "size":"1073741824", "modified":"1699000000" }
```

标准 `json.Unmarshal` 遇到「目标 int64、来源是字符串」直接返回类型错误，整段列表解析失败 —— 于是 `ListDrives()` / `testStorage` 全部抛错。

## 三、修复

- **`internal/openlist/client.go`**：新增 `FlexInt64` 类型（`type FlexInt64 int64`），实现 `UnmarshalJSON` / `MarshalJSON`，兼容 OpenList 把数值字段混用以下任意形式的真实返回：
  - 数字（`1699000000`）
  - 字符串数字（`"1699000000"`、`"1073741824"`）
  - 浮点（`1699000000.0`、`1.5e3`）
  - ISO 日期串（`2023-11-03T07:46:40Z` → 转 unix 秒）
  - 空 / 缺省 / 解析不出 → 回退 `0`，**绝不因单条脏数据让整次列表或测试连接失败**。
  - `FsObj.Size` 与 `FsObj.Modified` 均改为 `FlexInt64`（同类问题一并根治，避免下一个「size 也是字符串」的坑）。
- **`internal/scanner/scanner.go`**：落库处 `int64(o.Size)` / `int64(o.Modified)` 做类型转换（边界处归一回 `int64`，`model.MediaFile.Modified` 类型不变）。
- **`internal/api/handlers.go`**：`Version` 升到 `1.1.4`。
- 新增 `internal/openlist/client_flex_test.go`：覆盖「全数字 / modified 为字符串 / size+modified 均为字符串 / 浮点 / ISO 日期 / 缺省回退 0」以及「列表里有一个对象字段串化也不应中断整次解析」。

## 四、验证

- `go build ./...` / `go test ./...` 全绿（含新增 7 个 openlist 用例）。
- **端到端实测**（mock OpenList 返回 `modified`/`size` 均为字符串，复现用户报错场景）：
  - 修复前必然报 `cannot unmarshal string into ... modified of type int64`；
  - 修复后 `POST /api/storages/test` 返回 `{"ok":true,"drives":[...]}`，且 `modified`、`size` 已正确解析为数字（`1699000000`、`1073741824`）。

## 五、升级

```bash
docker compose pull && docker compose up -d
```

升级后「测试连接」应能正常列出已挂载网盘；如仍报错，请贴出完整错误信息（现网盘列表能拿到了，多数情况已解决）。

## 六、镜像

```
tianjian518/newmovie:1.1.4
tianjian518/newmovie:latest
```

架构：`linux/amd64` · `linux/arm64` · `linux/arm/v7`
