package openlist

// FSClient 是媒体库扫描与播放所依赖的最小存储驱动接口。
//
// 设计目的：Vidrive 的「列表 / 取直链 / 读小文件」逻辑目前只面向 OpenList 实现，
// 但同一套消费逻辑（scanner 扫描目录、play 取链、NFO 读取）完全适用于 WebDAV
// 等其他存储。把它抽象成接口后，*Client（OpenList 实现）天然满足，
// internal/webdav 的 WebDAV 实现也满足，消费方只需面向接口编程。
//
// 注意：WebDAV 没有 OpenList 的「签名 /d/ 中转」概念，SignedDURL 在 WebDAV
// 实现里直接返回直链本身；IsOpenListD 相关归一化仅 OpenList 需要。
type FSClient interface {
	// List 列出某路径下的内容。refresh 对 OpenList 是缓存开关；WebDAV 忽略。
	List(path string, refresh bool) ([]FsObj, error)
	// GetLink 取某文件的直链信息。OpenList 返回 raw_url + /d/ url；
	// WebDAV 返回 raw_url = 服务端直链（本机直读或经容器挂载）。
	GetLink(path string, refresh bool) (*FsGetResp, error)
	// RawURL 取内部路径的真实直链（失败回退 SignedDURL），用于代理读小文件。
	RawURL(path string) (string, error)
	// SignedDURL 生成可直接请求的 URL。OpenList 是带签名的 /d/；WebDAV 即直链。
	SignedDURL(path string) string
	// ListDrives 列出根目录（OpenList 为已挂载网盘，WebDAV 为根集合）。
	ListDrives() ([]FsObj, error)
	// ReadText 通过直链读取一个小文本文件（NFO 等），上限 1MB。
	ReadText(path string) (string, error)
}

// 编译期断言：*Client 实现 FSClient。
var _ FSClient = (*Client)(nil)
