import { useEffect, useState } from "react";
import { api, isAdmin } from "../api";
import type { Storage, PathRewrite } from "../types";

// 设置页：绑定 OpenList 存储源（含连通性测试）、管理 strm 路径重写规则、填 TMDB Key。
export default function Settings() {
  const admin = isAdmin();
  return (
    <div className="space-y-10">
      <TranscodePanel />
      {admin && <TmdbPanel />}
      {admin && <StoragePanel />}
      {admin && <RewritePanel />}
      {admin && <StrmTips />}
      {!admin && (
        <p className="text-sm text-gray-500">
          存储源管理、路径重写、TMDB Key 等设置仅管理员可见。
        </p>
      )}
    </div>
  );
}

// TMDB Key：用户自填并持久化（环境变量未设置时生效）。留空则仅靠 NFO + 同目录图刮削。
// 「测试」按钮会真发一次搜索，提前暴露「Key 无效」与「网络连不上 TMDB」两类问题。
function TmdbPanel() {
  const [key, setKey] = useState("");
  const [base, setBase] = useState("");
  const [msg, setMsg] = useState("");
  const [testing, setTesting] = useState(false);

  const load = () =>
    api.getSettings().then((s) => {
      setKey(s["tmdb_api_key"] || "");
      setBase(s["tmdb_api_base"] || "");
    }).catch(() => {});
  useEffect(() => { load(); }, []);

  async function save() {
    await api.saveSettings({ tmdb_api_key: key, tmdb_api_base: base });
    setMsg("已保存（重启无需重填；留空则跳过 TMDB）");
  }

  async function test() {
    setTesting(true);
    setMsg("测试中…");
    try {
      const r = await api.testTmdb(key, base);
      setMsg(`连接正常：命中「${r.sample}」，实际使用 ${r.endpoint}`);
    } catch (e: any) {
      setMsg("失败：" + (e?.message || e));
    } finally {
      setTesting(false);
    }
  }

  return (
    <section>
      <h2 className="text-xl font-bold mb-3">TMDB 刮削</h2>
      <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl">
        <input className="w-full bg-ink rounded p-2" type="password" placeholder="在 themoviedb.org 申请的 v3 API Key（仅自己用，存于本机）" value={key} onChange={(e) => setKey(e.target.value)} />
        <input className="w-full bg-ink rounded p-2" placeholder="API 地址（可留空，仅在自建反代时填，如 https://tmdb.example.com/3）" value={base} onChange={(e) => setBase(e.target.value)} />
        <div className="flex gap-2">
          <button onClick={save} className="bg-brand rounded px-3 py-1 text-sm">保存</button>
          <button onClick={test} disabled={testing} className="bg-ink rounded px-3 py-1 text-sm disabled:opacity-50">测试连接</button>
        </div>
        {msg && <p className="text-sm text-gray-400">{msg}</p>}
      </div>
      <p className="text-sm text-gray-400 mt-2">填了 Key 后，缺 NFO 的影片会自动从 TMDB 补海报/简介/评分。已刮削过的条目不会重复请求（增量缓存）。</p>
      <p className="text-sm text-gray-400">部分网络连不上 api.themoviedb.org，程序会自动改用备用地址 api.tmdb.org；都不通时再填上面的自建反代地址。</p>
    </section>
  );
}

// 视频转码开关 + ffmpeg 状态。ffmpeg 是重封装/转码的基础；
// 服务端装了 ffmpeg 时转码默认开启（HEVC 也能页内播），用户可在此关闭以省 CPU。
// 服务端没装 ffmpeg：重封装/转码都不可用，MKV/HEVC 只能外部播放，这里给出明确提示。
function TranscodePanel() {
  const [on, setOn] = useState(false);
  const [ffmpegOk, setFfmpegOk] = useState<boolean | null>(null);
  const [transcodeOk, setTranscodeOk] = useState<boolean | null>(null);
  const [msg, setMsg] = useState("");

  useEffect(() => {
    // 有效状态 = 用户设置（transcode_enabled）优先，否则取服务端默认（ffmpeg+libx264 在则默认开）。
    Promise.all([api.getSettings(), api.health()])
      .then(([s, h]) => {
        setFfmpegOk(!!h.ffmpeg_ok);
        setTranscodeOk(!!h.transcode_ok);
        setOn(s["transcode_enabled"] === "1" || (s["transcode_enabled"] == null && !!h.transcode));
      })
      .catch(() => {});
  }, []);

  // 转码可用 = ffmpeg 在且带 libx264。缺 libx264 时勾选无意义，禁用并提示。
  const canTranscode = ffmpegOk === true && transcodeOk === true;

  async function toggle() {
    const next = !on;
    setOn(next);
    await api.saveSettings({ transcode_enabled: next ? "1" : "0" });
    setMsg(next
      ? "已开启：HEVC 等编码将转成 H.264 页内播放（更吃 CPU，但任何浏览器都能播）"
      : "已关闭：恢复「仅 HEVC 能力浏览器可播」的轻量重封装");
  }

  return (
    <section>
      <h2 className="text-xl font-bold mb-3">视频转码（页内播放兜底）</h2>
      <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl">
        <div className="flex items-center gap-2 text-sm">
          <span className={"inline-block w-2.5 h-2.5 rounded-full " + (ffmpegOk ? "bg-green-400" : ffmpegOk === false ? "bg-red-400" : "bg-gray-500")} />
          <span className="text-gray-300">
            {ffmpegOk === null ? "检测服务端能力中…" : ffmpegOk ? "ffmpeg 已就绪：重封装可用" + (transcodeOk ? "，转码可用" : "，但缺少 libx264（转码不可用）") : "ffmpeg 未安装：MKV / HEVC 只能唤起外部播放器"}
          </span>
        </div>
        {ffmpegOk === false && (
          <p className="text-sm text-amber-200 bg-amber-500/10 border border-amber-500/40 rounded p-3">
            服务端缺少 ffmpeg，重封装 / 转码均不可用。请重新部署含 ffmpeg 的镜像
            （<code className="bg-black/30 px-1 rounded">tianjian518/newmovie:latest</code> 已含 ffmpeg）后，MKV / HEVC 即可页内播放。
          </p>
        )}
        {ffmpegOk === true && transcodeOk === false && (
          <p className="text-sm text-amber-200 bg-amber-500/10 border border-amber-500/40 rounded p-3">
            ffmpeg 缺少 libx264 编码器，视频转码（HEVC→H.264）不可用。HEVC 内容将走重封装保留 HEVC
            （仅 HEVC 能力浏览器可播）。请换含 libx264 的 ffmpeg 构建以启用通用转码。
          </p>
        )}
        <label className="flex items-start gap-3 cursor-pointer">
          <input type="checkbox" className="mt-1" checked={on} onChange={toggle} disabled={!canTranscode} />
          <span>
            <span className="font-medium">允许视频转码（HEVC→H.264）</span>
            <p className="text-sm text-gray-400 mt-1">
              网盘里的 MKV / 4K 蓝光原盘多为 HEVC，多数浏览器（尤其 Firefox、多数 Chrome）解不了 → 重封装后仍放不出。
              开启后服务端实时转成 H.264，<b>任何浏览器都能页内播</b>；代价是更吃 CPU（4K 明显）。
              这正是「无论什么 strm 都能页内播放」的通用路径。
            </p>
          </span>
        </label>
        {msg && <p className="text-sm text-gray-400">{msg}</p>}
      </div>
    </section>
  );
}

function StoragePanel() {
  const [list, setList] = useState<Storage[]>([]);
  const [editingID, setEditingID] = useState("");
  const [form, setForm] = useState({ name: "", type: "openlist", base_url: "http://openlist:5244", token: "", sign_key: "", rate_limit: 2, local_root: "" });
  const [msg, setMsg] = useState("");

  const load = () => api.listStorages().then(setList).catch(() => {});
  useEffect(() => { load(); }, []);

  function resetForm() {
    setEditingID("");
    setForm({ name: "", type: "openlist", base_url: "http://openlist:5244", token: "", sign_key: "", rate_limit: 2, local_root: "" });
    setMsg("");
  }
  function edit(s: Storage) {
    setEditingID(s.id);
    setForm({ name: s.name, type: s.type, base_url: s.base_url, token: s.token, sign_key: s.sign_key, rate_limit: s.rate_limit, local_root: s.local_root || "" });
    setMsg("");
  }
  async function test() {
    setMsg("测试中…");
    try {
      const r = await api.testStorage({ base_url: form.base_url, token: form.token, sign_key: form.sign_key });
      setMsg("连接成功，已挂载网盘：" + r.drives.map((d: any) => d.name).join("、"));
    } catch (e: any) {
      setMsg("失败：" + e.message);
    }
  }
  async function save() {
    if (!form.name || !form.base_url) {
      setMsg("请填写名称和 Base URL");
      return;
    }
    if (editingID) {
      await api.updateStorage(editingID, form);
      setMsg("已更新");
    } else {
      await api.createStorage(form);
      setMsg("已保存");
    }
    resetForm();
    load();
  }
  async function del(id: string) {
    await api.deleteStorage(id);
    if (editingID === id) resetForm();
    load();
  }

  return (
    <section>
      <h2 className="text-xl font-bold mb-3">存储源（OpenList）</h2>
      <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl">
        <input className="w-full bg-ink rounded p-2" placeholder="名称（如 openlist，必填）" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" placeholder="Base URL（同 Docker 网络用 http://openlist:5244，不要填 localhost）" value={form.base_url} onChange={(e) => setForm({ ...form, base_url: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" placeholder="API Token" value={form.token} onChange={(e) => setForm({ ...form, token: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" placeholder="签名密钥（后台『签名所有』，留空表示关闭签名）" value={form.sign_key} onChange={(e) => setForm({ ...form, sign_key: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" type="number" placeholder="限速 req/s（默认 2）" value={form.rate_limit} onChange={(e) => setForm({ ...form, rate_limit: Number(e.target.value) })} />
        <input className="w-full bg-ink rounded p-2" placeholder="本地挂载根（可选）。如 /mnt/cloud。与 .strm 里写的本地路径前缀一致即可让本地路径型 strm 页内播放" value={form.local_root} onChange={(e) => setForm({ ...form, local_root: e.target.value })} />
        <div className="flex gap-2">
          <button onClick={test} className="bg-panel rounded px-3 py-1 text-sm">测试连接</button>
          <button onClick={save} className="bg-brand rounded px-3 py-1 text-sm">{editingID ? "更新" : "保存"}</button>
          {editingID && <button onClick={resetForm} className="bg-panel rounded px-3 py-1 text-sm">取消</button>}
        </div>
        {msg && <p className="text-sm text-gray-400">{msg}</p>}
      </div>
      <ul className="mt-3 text-sm text-gray-400 space-y-1">
        {list.length === 0 && <li>还没有存储源，填上面表单并保存。</li>}
        {list.map((s) => (
          <li key={s.id} className="flex items-center justify-between bg-ink/40 rounded px-2 py-1">
            <button className="text-left hover:text-gray-100" onClick={() => edit(s)}>{s.name} · {s.base_url} · {s.type}</button>
            <button className="text-red-400 hover:text-red-300 ml-2" onClick={() => del(s.id)}>删除</button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function RewritePanel() {
  const [list, setList] = useState<PathRewrite[]>([]);
  const [form, setForm] = useState({ pattern: "", replacement: "", priority: 100 });

  const load = () => api.listRewrites().then(setList).catch(() => {});
  useEffect(() => { load(); }, []);

  return (
    <section>
      <h2 className="text-xl font-bold mb-3">STRM 路径重写规则</h2>
      <p className="text-sm text-gray-400 mb-2">存量 strm 里写死 <code>http://localhost:5244/...</code>（容器内不通）？配一条正则即可，不必重生成几万个文件。</p>
      <div className="bg-card rounded-lg p-4 space-y-3 max-w-xl">
        <input className="w-full bg-ink rounded p-2" placeholder="正则，如 ^http://localhost:5244/d/(.*)$" value={form.pattern} onChange={(e) => setForm({ ...form, pattern: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" placeholder="替换，如 openlist://main/$1" value={form.replacement} onChange={(e) => setForm({ ...form, replacement: e.target.value })} />
        <input className="w-full bg-ink rounded p-2" type="number" placeholder="优先级（小者先匹配）" value={form.priority} onChange={(e) => setForm({ ...form, priority: Number(e.target.value) })} />
        <button onClick={async () => { await api.createRewrite(form); setForm({ pattern: "", replacement: "", priority: 100 }); load(); }} className="bg-brand rounded px-3 py-1 text-sm">添加规则</button>
      </div>
      <ul className="mt-3 text-sm text-gray-400 space-y-1">
        {list.map((r) => <li key={r.id}>P{r.priority} · <code>{r.pattern}</code> → <code>{r.replacement}</code></li>)}
      </ul>
    </section>
  );
}

function StrmTips() {
  return (
    <section>
      <h2 className="text-xl font-bold mb-3">STRM 方言支持</h2>
      <ul className="text-sm text-gray-400 list-disc list-inside space-y-1">
        <li>完整直链：<code>http://nas:5244/d/quark/电影/x.mkv</code></li>
        <li>带签名：上述 URL 追加 <code>?sign=xxx:0</code>（自动忽略，NewMovie 重算）</li>
        <li>URL 编码（Encode Path）：<code>/d/.../%E7%94%B5%E5%BD%B1/...</code>（自动解码）</li>
        <li>纯内部路径（Without Url）：<code>/quark/电影/x.mkv</code></li>
        <li>本地绝对路径（CloudDrive2）：<code>/mnt/cd2/quark/x.mkv</code></li>
        <li>相对路径（Kodi）：<code>流浪地球.mkv</code></li>
      </ul>
    </section>
  );
}
