package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// jsSandboxPrelude 是 node 侧沙箱前置：把 app.js 读进 src、装惰性 DOM 桩、建 vm
// context。两个 JS 闸门（顶层冒烟 / 函数体断言）共用同一份桩——各写一份的话，
// 「冒烟过了但断言跑在另一个环境里」这种偏差本身就会成为新的坑。
// app.js 是平铺脚本（无顶层 IIFE），其顶层 function 声明因此挂到 context 全局，
// 断言脚本可直接调 sandbox.fmtHit 之类。
// 沙箱对未列出的全局量一律返回 undefined（get 陷阱只看 target 自有属性），
// NaN / Infinity / undefined 这三个内建量真实浏览器都有，漏掉会让「NaN 判定」
// 这类断言在沙箱里假绿/假红——按值补上，保持与浏览器同判。
const jsSandboxPrelude = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
// 沙箱是个**普通对象**而不是 Proxy：严格模式下"给未声明变量赋值/读未声明量"是
// ReferenceError，浏览器会炸，普通对象的全局作用域天然同判（找不到绑定即抛）。
// 曾用 Proxy + has(){return true} 兜成 undefined —— 那正好把"删掉声明却漏了赋值"
// 这类残留引用静默吞掉（v1.11.8 上线即炸的 usLast 就是这么漏过去的）。代价是
// 用到的浏览器全局必须在此**显式列全**：漏了会在测试里以 ReferenceError 现形，
// 正是要的效果（与浏览器同判），补上即可。
const sandbox = {
  NaN: NaN, Infinity: Infinity, undefined: undefined,
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  confirm: () => true, alert() {},
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, isFinite, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
};
// window/globalThis 在浏览器里就是全局对象本身（自引用），这样 window.isSecureContext
// 之类的读取与浏览器同解。
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
`

// runNodeJS 把一段 node 脚本写盘并执行，返回合并输出；失败（非 0 退出）由调用方判定。
func runNodeJS(t *testing.T, script, smokeHash string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS checks skipped")
	}
	hf, err := os.CreateTemp(t.TempDir(), "js-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(script); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	cmd := exec.Command(node, hf.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	cmd.Env = append(os.Environ(), "SMOKE_HASH="+smokeHash)
	out, _ := cmd.CombinedOutput()
	return out
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	harness := jsSandboxPrelude + `try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		out := runNodeJS(t, harness, hash)
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js 顶层求值 %s 崩溃:\n%s", hash, out)
		}
	}
}

// TestUsageCacheFormatters 用量视图两列新字段的判空口径：命中率与实付单价必须能
// 区分「无样本」与「值为 0」——把「上游没报缓存口径」读成「命中率 0%」会误导运维
// 去查缓存失效，而实际只是老响应没这个字段。语法检查与顶层冒烟都不执行函数体，
// 这条是唯一会走到 fmtHit/fmtUnit 里的闸门。
func TestUsageCacheFormatters(t *testing.T) {
	harness := jsSandboxPrelude + `vm.runInContext(src, sandbox, { filename: 'app.js' });
const cases = [
  ['fmtHit', {}, '—'],
  ['fmtHit', { cache_samples: 0, cache_hit_tokens: 0, cache_miss_tokens: 0 }, '—'],
  ['fmtHit', { cache_samples: 1, cache_hit_tokens: 0, cache_miss_tokens: 60, cache_hit_rate: 0 }, '0.0%'],
  ['fmtHit', { cache_samples: 2, cache_hit_tokens: 900, cache_miss_tokens: 100, cache_hit_rate: 0.9 }, '90.0%'],
  ['fmtUnit', { credit_samples: 0, total_tokens: 1000, credits: 5 }, '—'],
  ['fmtUnit', { credit_samples: 1, total_tokens: 0, credits: 5 }, '—'],
  ['fmtUnit', { credit_samples: 1, total_tokens: 251049665, credits: 227.08 }, '0.90'],
  ['fmtUnit', { credit_samples: 1, total_tokens: 25554987, credits: 1664.86 }, '65.15'],
  ['fmtUnit', { credit_samples: 1, total_tokens: 1000000, credits: 159.82 }, '160'],
];
let bad = 0;
for (const [fn, arg, want] of cases) {
  const got = sandbox[fn](arg);
  if (got !== want) { console.log('FAIL ' + fn + '(' + JSON.stringify(arg) + ') = ' + JSON.stringify(got) + ', want ' + JSON.stringify(want)); bad++; }
}
console.log(bad ? 'FORMAT FAIL ' + bad : 'FORMAT OK');
process.exit(bad ? 1 : 0);
`
	out := runNodeJS(t, harness, "#usage")
	if !bytes.Contains(out, []byte("FORMAT OK")) {
		t.Fatalf("用量列格式化断言未通过:\n%s", out)
	}
}

// TestUsageDailySeries 按天视图的三块纯逻辑：dailyWindow（窗口按**整日**裁剪）、
// usagePoints（时序点取值与脏点过滤）、dayWindowHours（把选择器换算成后端认的
// 整点小时数）。语法检查与顶层冒烟都不执行函数体，而"近 24 小时"到底含不含
// 昨天、换算后后端的裁剪线是不是真落在午夜（落在午夜才能保证卡片/表/图同一批桶、
// 合计相等），这类边界错在界面上只表现为"数字差一点"，肉眼极难发现——这条是唯一
// 会走到这几个函数里的闸门。
func TestUsageDailySeries(t *testing.T) {
	harness := jsSandboxPrelude + `vm.runInContext(src, sandbox, { filename: 'app.js' });
let bad = 0;
const eq = (what, got, want) => {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) { console.log('FAIL ' + what + ' = ' + g + ', want ' + w); bad++; }
};
// 本地日期字符串（与后端 day 分片同格式）；-30 是"窗口外的老数据"。
const ymd = off => {
  const d = new Date();
  d.setDate(d.getDate() + off);
  return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
};
const day = off => ({ t: ymd(off), scope: 'day', prompt_tokens: 1, completion_tokens: 1, total_tokens: 2, requests: 1 });
const keys = arr => arr.map(p => p.t).join(',');
const list = [day(-30), day(-2), day(-1), day(0)];

eq('全部历史不裁', keys(sandbox.dailyWindow(list, 0)), keys(list));
// 「近 N 小时」在日粒度下 = 含今天在内的 ceil(N/24) 个自然日，与跑测试的钟点无关。
eq('近24小时=今天', keys(sandbox.dailyWindow(list, 24)), ymd(0));
eq('近3天=含今天3个自然日', keys(sandbox.dailyWindow(list, 72)), [ymd(-2), ymd(-1), ymd(0)].join(','));
eq('近7天只留窗口内', keys(sandbox.dailyWindow(list, 168)), [ymd(-2), ymd(-1), ymd(0)].join(','));
// 边界按**日期**算，不是按点数：没请求的日子不产生桶，断档不能把窗口撑大。
eq('断档不改变日窗边界', keys(sandbox.dailyWindow([day(-40), day(-1), day(0)], 72)), [ymd(-1), ymd(0)].join(','));
// 近 30 天 = 含今天在内的 30 个自然日 → 今天-29 在窗口内，今天-30 在外。
eq('近30天含今天-29', keys(sandbox.dailyWindow([day(-30), day(-29)], 720)), ymd(-29));
eq('近30天不含今天-30', keys(sandbox.dailyWindow(list, 720)), [ymd(-2), ymd(-1), ymd(0)].join(','));
// 时间解析失败的点不进窗口（渲染器另有兜底过滤，这里是第一道）。
eq('脏点不参与窗口', keys(sandbox.dailyWindow([{ t: 'garbage', scope: 'day' }].concat(list), 24)), ymd(0));
eq('窗口内无数据给空数组', sandbox.dailyWindow([day(-30)], 24).length, 0);
eq('字符串小时数也可用', keys(sandbox.dailyWindow(list, '72')), [ymd(-2), ymd(-1), ymd(0)].join(','));

// dayWindowHours：换算后发给后端的小时数，必须让后端的裁剪线（nowHour-(hours-1)）
// 正好落在第 N 个自然日的 00:00 —— 这是「按天图合计 == 卡片 == 三张表」的前提
//（曾出现图=整日 636M、表=滚动24h 1037M 的矛盾）。后端公式在此复刻一份断言等价。
const backendCut = hrs => {
  const nowHour = new Date();
  nowHour.setMinutes(0, 0, 0);
  return new Date(nowHour.getTime() - (hrs - 1) * 3600000);
};
for (const c of [[24, 1], [72, 3], [168, 7], [720, 30], ['72', 3]]) {
  const sel = c[0], wantDays = c[1];
  const hrs = sandbox.dayWindowHours(sel);
  eq('sel=' + sel + ' 天数', sandbox.dayWindowDays(sel), wantDays);
  eq('sel=' + sel + ' 换算后裁剪线=午夜', backendCut(hrs).getTime(), sandbox.dayWindowStart(sel).getTime());
  eq('sel=' + sel + ' 裁剪线落在整点', backendCut(hrs).getMinutes(), 0);
  eq('sel=' + sel + ' 是正整数小时', hrs >= 1 && Math.floor(hrs) === hrs, true);
}
eq('全部历史不换算', sandbox.dayWindowHours(0), 0);
eq('全部历史没有起点', sandbox.dayWindowStart(0), null);
eq('全部历史天数为 0', sandbox.dayWindowDays(0), 0);
// 近 7 天起点 = 今天往前 6 个自然日的午夜（含今天共 7 天）
const exp7 = new Date();
exp7.setHours(0, 0, 0, 0);
exp7.setDate(exp7.getDate() - 6);
eq('近7天起点=今天-6 的午夜', sandbox.dayWindowStart(168).getTime(), exp7.getTime());
// 换算出的小时数必须在后端 1440 上限内（近30天 ≈ 721），否则会被静默截断成 60 天
eq('近30天不触后端上限', sandbox.dayWindowHours(720) <= 1440, true);

const pts = sandbox.usagePoints([
  { t: '2026-09-16T13', scope: 'hour', prompt_tokens: 10, completion_tokens: 5, total_tokens: 20, requests: 3, credits: 1.25, credit_samples: 2 },
  { t: '2026-09-16', scope: 'day', prompt_tokens: 7, completion_tokens: 0, requests: 1 },
  { t: 'nope', scope: 'hour', prompt_tokens: 1 },
]);
eq('脏点被丢掉', pts.length, 2);
eq('hour 点取值', [pts[0].pt, pts[0].ct, pts[0].tt, pts[0].req, pts[0].cr, pts[0].crn, pts[0].scope],
  [10, 5, 20, 3, 1.25, 2, 'hour']);
eq('缺 total 用 pt+ct 兜底、缺 credit 记 0 样本', [pts[1].tt, pts[1].crn, pts[1].scope], [7, 0, 'day']);
eq('day 点按当天 00:00 定位', new Date(pts[1].t).getHours(), 0);

console.log(bad ? 'DAILY FAIL ' + bad : 'DAILY OK');
process.exit(bad ? 1 : 0);
`
	out := runNodeJS(t, harness, "#usage")
	if !bytes.Contains(out, []byte("DAILY OK")) {
		t.Fatalf("按天视图断言未通过:\n%s", out)
	}
}

// TestUsageChartStructure 时序图的 DOM 结构约定：
//  1. 每根柱子的 <title> 必须在 <g> 里（挂成 <svg> 的直接子节点时，浏览器把
//     第一条当成整个 SVG 的标题——悬停任何柱子都只弹第一根的信息；按天的宽柱
//     一眼穿帮，这条保证不会再退化）；
//  2. 按天视图不画跨天分隔线（每天都是新的一天，画出来是栅格噪音），按小时视图
//     跨天时必须有；
//  3. 全是失败尝试的分片（有请求、无 token）也要留一条可悬停的命中条。
func TestUsageChartStructure(t *testing.T) {
	harness := jsSandboxPrelude + `vm.runInContext(src, sandbox, { filename: 'app.js' });
let bad = 0;
const eq = (what, got, want) => {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) { console.log('FAIL ' + what + ' = ' + g + ', want ' + w); bad++; }
};
const ok = (what, cond) => { if (!cond) { console.log('FAIL ' + what); bad++; } };

const dayPts = [
  { t: '2026-09-26', scope: 'day', prompt_tokens: 418400000, completion_tokens: 1720000, total_tokens: 420120000, requests: 1876, credits: 2271.02, credit_samples: 1876 },
  { t: '2026-09-27', scope: 'day', prompt_tokens: 730000000, completion_tokens: 2487000, total_tokens: 732487000, requests: 2100, credits: 2679.44, credit_samples: 2100 },
  { t: '2026-09-28', scope: 'day', prompt_tokens: 556000000, completion_tokens: 2506000, total_tokens: 558506000, requests: 1800, credits: 900.5, credit_samples: 1800 },
];
const dsvg = sandbox.usageChartHTML(dayPts, 'day');

// 1) title 必须嵌在 <g> 内：把所有 <g>…</g> 摘掉后不应再有 title；且每组一个。
eq('按天组数=点数', (dsvg.match(/<g>/g) || []).length, 3);
eq('title 全在 <g> 内', dsvg.replace(/<g>[\s\S]*?<\/g>/g, '').indexOf('<title>'), -1);
const titles = (dsvg.match(/<title>[^<]*<\/title>/g) || []).map(s => s.slice(7, -8));
eq('每条提示各不相同', new Set(titles).size, 3);
ok('9-26 提示带自己的日期与次数', titles[0].indexOf('2026-09-26') === 0 && titles[0].indexOf('1876 次') > 0 && titles[0].indexOf('2271.02 积分') > 0);
ok('9-27 提示是自己的数据', titles[1].indexOf('2026-09-27') === 0 && titles[1].indexOf('2100 次') > 0);
ok('未上报 credit 的点不写积分段', sandbox.usageChartHTML([{ t: '2026-09-26', scope: 'day', prompt_tokens: 5, requests: 1 }], 'day').indexOf('积分') < 0);

// 2) 跨天分隔线：按天视图 0 条，按小时跨天 >0 条。
eq('按天不画跨天分隔线', (dsvg.match(/opacity:\.45/g) || []).length, 0);
const hsvg = sandbox.usageChartHTML([
  { t: '2026-09-26T23', scope: 'hour', total_tokens: 10, requests: 1 },
  { t: '2026-09-27T00', scope: 'hour', total_tokens: 20, requests: 1 },
], 'hour');
eq('按小时跨天画 1 条分隔线', (hsvg.match(/opacity:\.45/g) || []).length, 1);

// 3) 有请求无 token 的分片：留透明命中条 + 提示里能看到次数。
const failOnly = sandbox.usageChartHTML([{ t: '2026-09-26T03', scope: 'hour', requests: 4, errors: 4 }], 'hour');
ok('失败分片有透明命中条', failOnly.indexOf('fill="transparent"') > 0);
ok('失败分片提示含次数', failOnly.indexOf('4 次') > 0);
eq('失败分片仍只 1 个组', (failOnly.match(/<g>/g) || []).length, 1);

// 4) 空数据：按小时/按天给不同的空态文案，且不产出 svg。
ok('按天空态', sandbox.usageChartHTML([], 'day').indexOf('us-empty') > 0 &&
  sandbox.usageChartHTML([], 'day').indexOf('<svg') < 0);
ok('脏点被丢弃后也算空态', sandbox.usageChartHTML([{ t: 'garbage', scope: 'day' }], 'day').indexOf('us-empty') > 0);

console.log(bad ? 'CHART FAIL ' + bad : 'CHART OK');
process.exit(bad ? 1 : 0);
`
	out := runNodeJS(t, harness, "#usage")
	if !bytes.Contains(out, []byte("CHART OK")) {
		t.Fatalf("时序图结构断言未通过:\n%s", out)
	}
}

// TestUsageRenderSmoke 真正跑一遍 renderUsage 全链路（v1.11.8 上线即炸的回归闸门）。
//
// 为什么必须有这条：语法检查、顶层冒烟、纯函数断言都**不执行** renderUsage 的函数体，
// 而它里面的残留引用（删掉 usLast 声明却漏了 renderUsage 里的 usLast = d 赋值）在
// 严格模式下是 ReferenceError，被 loadUsage 的 catch 接住、只显示成"读取用量失败：
// usLast is not defined"——页面照样打开，测试照样全绿。把"渲染一次不许抛 + 渲染出
// 预期内容"钉死，这类问题再也到不了线上。
func TestUsageRenderSmoke(t *testing.T) {
	harness := jsSandboxPrelude + `vm.runInContext(src, sandbox, { filename: 'app.js' });

// 轻量 DOM：可读可写的普通节点（inert 桩会把 innerHTML 吞掉，无法断言内容）。
const lite = {};
const node = id => (lite[id] = lite[id] || {
  innerHTML: '', textContent: '', title: '', value: '', selectedOptions: [],
  options: [], querySelector: () => null, addEventListener() {},
  classList: { toggle() {} },
});
sandbox.document.getElementById = id => node(id);
sandbox.document.querySelectorAll = () => [];

const payload = {
  totals: { requests: 4126, errors: 226, prompt_tokens: 638000000, completion_tokens: 1970000,
            total_tokens: 639865872, avg_latency_ms: 14640, avg_tokens_per_second: 159.4,
            credits: 711.4, credit_samples: 4000, cache_hit_tokens: 620000000, cache_miss_tokens: 18000000,
            cache_samples: 4100, cache_hit_rate: 0.972 },
  by_account: [{ key: 'd518b7980000', extra: '😘 九月', realm: 'cn', requests: 1400, errors: 22,
                 prompt_tokens: 385920000, completion_tokens: 1160000, total_tokens: 387080000,
                 credits: 1188.62, credit_samples: 1350, cache_hit_tokens: 380000000, cache_miss_tokens: 6000000,
                 cache_samples: 1400, cache_hit_rate: 0.975, avg_latency_ms: 15130, avg_tokens_per_second: 159.4 }],
  by_model: [{ key: 'cn:deepseek-v4.1-flash', requests: 1400, errors: 22, prompt_tokens: 385920000,
               completion_tokens: 1160000, total_tokens: 387080000, credits: 1188.62, credit_samples: 1350,
               cache_hit_tokens: 380000000, cache_miss_tokens: 6000000, cache_samples: 1400,
               cache_hit_rate: 0.975 }],
  by_realm: [{ key: 'cn', requests: 4126, errors: 226, prompt_tokens: 638000000,
               completion_tokens: 1970000, total_tokens: 639865872, credits: 711.4, credit_samples: 4000,
               cache_hit_tokens: 620000000, cache_miss_tokens: 18000000, cache_samples: 4100, cache_hit_rate: 0.972 }],
  series: [
    { t: '2026-09-27T22', scope: 'hour', requests: 120, prompt_tokens: 30000000, completion_tokens: 90000,
      total_tokens: 30090000, credits: 40, credit_samples: 120 },
    { t: '2026-09-28T01', scope: 'hour', requests: 80, prompt_tokens: 12000000, completion_tokens: 40000,
      total_tokens: 12040000, credits: 12, credit_samples: 80 },
  ],
  daily: [
    { t: '2026-09-27', scope: 'day', requests: 2907, prompt_tokens: 730130000, completion_tokens: 2357000,
      total_tokens: 732487000, credits: 2679.44, credit_samples: 2907 },
    { t: '2026-09-28', scope: 'day', requests: 1979, prompt_tokens: 556690000, completion_tokens: 1816000,
      total_tokens: 558506000, credits: 606.45, credit_samples: 1979 },
  ],
  models: ['cn:deepseek-v4.1-flash', 'cn:glm-5.3'],
  accounts: [{ uid: 'd518b7980000', nickname: '6' }, { uid: '006d1c2a0000', nickname: '😘 九月' }],
  buckets: 41, since: '2026-09-26T01', file_bytes: 56364, generated: '2026-09-28T17:00:00+08:00',
};

let bad = 0;
const ok = (what, cond) => { if (!cond) { console.log('FAIL ' + what); bad++; } };

// 按小时 + 按天两种粒度都要能整条链路跑通且不抛（残留未声明变量=严格模式 ReferenceError）
for (const gran of ['hour', 'day']) {
  try {
    vm.runInContext('usGran = ' + JSON.stringify(gran), sandbox);
    sandbox.renderUsage(payload);
  } catch (e) {
    console.log('FAIL renderUsage(' + gran + ') 抛异常: ' + (e && e.stack ? e.stack : e));
    bad++;
    continue;
  }
  ok('卡片 ' + gran, lite.usStats.innerHTML.indexOf('请求数') > 0 && lite.usStats.innerHTML.indexOf('711.4') > 0);
  ok('按账号表 ' + gran, lite.usAccBody.innerHTML.indexOf('d518b798') > 0 && lite.usAccBody.innerHTML.indexOf('😘 九月') > 0);
  ok('按模型表 ' + gran, lite.usModelBody.innerHTML.indexOf('cn:deepseek-v4.1-flash') > 0);
  ok('按域表 ' + gran, lite.usRealmBody.innerHTML.indexOf('>cn<') > 0);
  ok('下拉选项 ' + gran, lite.usModel.innerHTML.indexOf('cn:glm-5.3') > 0 &&
    lite.usAccount.innerHTML.indexOf('d518b7980000') > 0);
  ok('图 ' + gran, lite.usChart.innerHTML.indexOf('<svg') === 0 && lite.usChart.innerHTML.indexOf('<title>') > 0);
  ok('说明行 ' + gran, lite.usNote.textContent.length > 10);
  ok('说明行含口径解释 ' + gran, lite.usNote.title.indexOf('实测扣费') > 0);
}
// 按天模式的说明行必须写明实际范围（整日口径），且选择器标签改标「今天」
vm.runInContext('usGran = "day"', sandbox);
sandbox.renderUsage(payload);
ok('按天说明行写实际范围', /自 \d{2}-\d{2} 00:00 起/.test(lite.usNote.textContent));
vm.runInContext('usGran = "hour"', sandbox);
sandbox.renderUsage(payload);
ok('按小时说明行写分桶口径', lite.usNote.textContent.indexOf('个分桶') > 0);

// 空数据 / 脏数据 / 缺字段：不许抛
try {
  sandbox.renderUsage({ totals: {}, by_account: [], by_model: [], by_realm: [],
                        series: [{ t: 'garbage', scope: 'hour' }], daily: [{ t: 'garbage', scope: 'day' }],
                        models: [], accounts: [] });
  ok('空数据渲染出空态', lite.usChart.innerHTML.indexOf('us-empty') > 0);
} catch (e) {
  console.log('FAIL 空数据渲染抛异常: ' + e);
  bad++;
}
try {
  sandbox.renderUsage({});
  ok('完全空负载可渲染', true);
} catch (e) {
  console.log('FAIL 空负载渲染抛异常: ' + e);
  bad++;
}

console.log(bad ? 'RENDER FAIL ' + bad : 'RENDER OK');
process.exit(bad ? 1 : 0);
`
	out := runNodeJS(t, harness, "#usage")
	if !bytes.Contains(out, []byte("RENDER OK")) {
		t.Fatalf("用量渲染冒烟未通过:\n%s", out)
	}
}

// TestModelsTableSort 模型表两列排序（模型 / 积分倍率）的三态循环、倍率原文解析与
// 排序结果。语法检查与顶层冒烟都不执行函数体，而排序错位很难靠肉眼在 18 行表里
// 发现（尤其「生效价 vs 牌价」这种口径错，看着只是顺序怪）——这条是唯一会走到
// mdNextSort / mdCreditValue / mdSortList 里的闸门。
func TestModelsTableSort(t *testing.T) {
	harness := jsSandboxPrelude + `vm.runInContext(src, sandbox, { filename: 'app.js' });
let bad = 0;
const eq = (what, got, want) => {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) { console.log('FAIL ' + what + ' = ' + g + ', want ' + w); bad++; }
};
const ids = arr => arr.map(m => m.id);

// 1) 点击循环：默认 → 降序 → 升序 → 默认；点另一列即接管、从降序起。
let s = { key: '', dir: '' };
s = sandbox.mdNextSort(s, 'credits');
eq('倍率首点', s, { key: 'credits', dir: 'desc' });
s = sandbox.mdNextSort(s, 'credits');
eq('倍率二点', s, { key: 'credits', dir: 'asc' });
s = sandbox.mdNextSort(s, 'credits');
eq('倍率三点回默认', s, { key: '', dir: '' });
eq('跨列接管', sandbox.mdNextSort({ key: 'model', dir: 'asc' }, 'credits'), { key: 'credits', dir: 'desc' });

// 2) 倍率原文取数：以上游实际返回值为主（x 前缀 / 0x / 脏尾巴 / 空）。
eq('x0.29', sandbox.mdCreditValue({ credits: 'x0.29' }), 0.29);
eq('0x 折后免费', sandbox.mdCreditValue({ credits: '0x' }), 0);
eq('脏尾巴取第一个数', sandbox.mdCreditValue({ credits: 'x0.77 credits' }), 0.77);
eq('空倍率给 NaN', Number.isNaN(sandbox.mdCreditValue({ credits: '' })), true);
eq('字段全缺给 NaN', Number.isNaN(sandbox.mdCreditValue({})), true);
// 有折扣时按生效价（列里显示的大字），无折扣时按牌价
eq('折扣生效价优先', sandbox.mdCreditValue({ credits: 'x0.29', promo_factor: 0, promo_credits: '0x' }), 0);
eq('只有标签不带 factor 仍按牌价',
  sandbox.mdCreditValue({ credits: 'x0.79', promo_label: '夜间折扣' }), 0.79);

// 3) 排序结果。cn:auto 无倍率（显示 —）：升降都沉底，不占首行。
const L = [
  { id: 'cn:b', credits: 'x0.5' },
  { id: 'cn:a', credits: 'x1.2' },
  { id: 'cn:auto', credits: '' },
  { id: 'cn:c', credits: 'x0.05' },
];
eq('倍率降序', ids(sandbox.mdSortList(L, { key: 'credits', dir: 'desc' })), ['cn:a', 'cn:b', 'cn:c', 'cn:auto']);
eq('倍率升序', ids(sandbox.mdSortList(L, { key: 'credits', dir: 'asc' })), ['cn:c', 'cn:b', 'cn:a', 'cn:auto']);
eq('倍率相同保持上游顺序',
  ids(sandbox.mdSortList([{ id: 'x', credits: 'x0.5' }, { id: 'y', credits: 'x0.5' }], { key: 'credits', dir: 'desc' })), ['x', 'y']);
eq('模型升序', ids(sandbox.mdSortList(L, { key: 'model', dir: 'asc' })), ['cn:a', 'cn:auto', 'cn:b', 'cn:c']);
eq('模型降序', ids(sandbox.mdSortList(L, { key: 'model', dir: 'desc' })), ['cn:c', 'cn:b', 'cn:auto', 'cn:a']);
eq('默认态还原上游顺序', ids(sandbox.mdSortList(L, { key: '', dir: '' })), ['cn:b', 'cn:a', 'cn:auto', 'cn:c']);
eq('排的是副本，不动原数组', ids(L), ['cn:b', 'cn:a', 'cn:auto', 'cn:c']);
// 版本号走数字自然序：glm-5.2 在 glm-5.10 之前（纯字典序会反过来）
eq('版本号自然序',
  ids(sandbox.mdSortList([{ id: 'cn:glm-5.10' }, { id: 'cn:glm-5.2' }], { key: 'model', dir: 'asc' })),
  ['cn:glm-5.2', 'cn:glm-5.10']);

console.log(bad ? 'SORT FAIL ' + bad : 'SORT OK');
process.exit(bad ? 1 : 0);
`
	out := runNodeJS(t, harness, "#models")
	if !bytes.Contains(out, []byte("SORT OK")) {
		t.Fatalf("模型表排序断言未通过:\n%s", out)
	}
}
