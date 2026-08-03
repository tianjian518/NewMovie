// 真实浏览器端到端验证：登录 NewMovie → 打开实际播放页 → 检查 <video> 是否真的能加载。
// 目的：确认「浏览器 <video> 401 黑屏」这个真实 bug 已经被 ?token= 修复。
const { chromium } = require('playwright');

const BASE = 'http://localhost:18096';
const FILE_ID = 'f-2be79f91c5c21d73';

(async () => {
  const browser = await chromium.launch({ executablePath: '/usr/bin/chromium', args: ['--no-sandbox', '--disable-dev-shm-usage', '--autoplay-policy=no-user-gesture-required'] });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });

  const consoleErrors = [];
  const pageErrors = [];
  const remuxStatus = [];
  const failedRequests = [];
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('response', (r) => {
    const u = r.url();
    if (u.includes('/api/play/remux') || u.includes('/api/play/transcode')) {
      remuxStatus.push(r.status() + ' ' + (u.split('?')[0]));
    }
  });
  page.on('requestfailed', (r) => failedRequests.push(r.url() + ' :: ' + (r.failure() ? r.failure().errorText : '')));

  const log = (s) => console.log(s);

  // 1) 登录
  await page.goto(BASE + '/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(800);
  // 登录表单：用户名 / 密码 占位符（与前端一致）
  const userSel = 'input[placeholder="用户名"], input[type="text"]';
  const passSel = 'input[placeholder="密码"], input[type="password"]';
  await page.fill(userSel, 'admin');
  await page.fill(passSel, 'admin');
  await page.click('button[type="submit"], form button');
  await page.waitForTimeout(1500);
  log('1. 登录后标题: ' + (await page.title()));

  // 2) 打开实际播放页
  await page.goto(BASE + '/play/' + FILE_ID, { waitUntil: 'networkidle' });
  await page.waitForTimeout(1500);
  log('2. 播放页标题: ' + (await page.title()));

  // 3) 等播放决策 + <video> 出现
  await page.waitForSelector('video', { timeout: 15000 }).catch(() => log('   [warn] 未找到 <video> 元素'));
  await page.waitForTimeout(2500);

  // 记录决策接口返回的 url 是否带 token（通过拦截不到，改从 video.src 判断）
  const videoInfo = await page.evaluate(() => {
    const v = document.querySelector('video');
    if (!v) return { found: false };
    return {
      found: true,
      src: v.currentSrc || v.src || '',
      readyState: v.readyState,        // 0..4
      networkState: v.networkState,    // 0..3
      error: v.error ? v.error.code : null,
      paused: v.paused,
      duration: v.duration,
    };
  });
  log('3. <video> 信息: ' + JSON.stringify(videoInfo));

  // 4) 等视频真正推进（canplay / playing），最多 20s
  let played = false;
  try {
    await page.waitForFunction(() => {
      const v = document.querySelector('video');
      return v && (v.readyState >= 2 || (v.error && v.error.code));
    }, { timeout: 20000 });
    played = true;
  } catch (e) {
    log('   [timeout] 20s 内未到达 readyState>=2');
  }

  // 最终再读一次状态
  const final = await page.evaluate(() => {
    const v = document.querySelector('video');
    if (!v) return { found: false };
    return {
      found: true,
      src: (v.currentSrc || v.src || '').slice(0, 120),
      hasToken: (v.currentSrc || v.src || '').includes('token='),
      readyState: v.readyState,
      networkState: v.networkState,
      error: v.error ? v.error.code : null,
      currentTime: v.currentTime,
      duration: isFinite(v.duration) ? v.duration : null,
    };
  });

  await page.screenshot({ path: '/workspace/play_verify.png', fullPage: false });

  log('=== 结果汇总 ===');
  log('remux/transcode 响应状态: ' + JSON.stringify(remuxStatus));
  log('失败请求(含 401 体): ' + (failedRequests.length ? JSON.stringify(failedRequests.slice(0, 5)) : '无'));
  log('最终 <video>: ' + JSON.stringify(final));
  log('到达 readyState>=2: ' + played);
  log('控制台错误: ' + (consoleErrors.length ? JSON.stringify(consoleErrors.slice(0, 5)) : '无'));
  log('页面异常: ' + (pageErrors.length ? JSON.stringify(pageErrors.slice(0, 5)) : '无'));

  // 判定
  const ok = final.found && final.hasToken && final.error === null && final.readyState >= 1 && !remuxStatus.some((s) => s.startsWith('401'));
  log('=== 判定: ' + (ok ? 'PASS ✅ 浏览器能播（401 黑屏已修复）' : 'FAIL ❌') + ' ===');

  await browser.close();
  process.exit(ok ? 0 : 1);
})().catch((e) => { console.error('SCRIPT ERROR:', e); process.exit(2); });
