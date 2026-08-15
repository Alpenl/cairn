#!/usr/bin/env node
// 依赖漏洞门禁 = pnpm audit + 一道「豁免范围没有悄悄变大」的护栏。
//
// 为什么不直接 `pnpm audit`：
//
// pnpm 的 auditConfig.ignoreGhsas 是**通告级**过滤，不是路径级的（pnpm 10.13
// 源码里只比对 github_advisory_id，整个 advisory 连同其下所有 findings.paths
// 一起丢弃）。也就是说：将来任何别的包引入同一条通告，会被同一个豁免连带放行，
// 而且不打印任何提示——「绿」会掩盖「有东西被放行了」。
//
// 护栏用的是 `pnpm audit --json` 的字段行为差异（已实测）：
//   advisories             —— **过滤后**，被豁免的不在里面 → 用作 gate 本体
//   actions[].resolves[]   —— 即便被豁免，**通告 id + 路径仍完整列着** → 护栏
//
// 护栏钉的是 **(通告 id, 路径) 集合**，不是计数、也不是纯路径：
//   * 计数挡不住替换——老路径消失 + 新路径引入，净计数不变；
//   * 纯路径挡不住「同一条链上又来一条新通告」——把新 GHSA 加进 ignoreGhsas
//     后路径集合不动，护栏和 gate 双绿。brace-expansion / minimatch 历史上
//     不止一条通告，这不是理论场景。
//
// 代价是这个集合**比计数更敏感**：依赖链中间跳数变化（比如 eslint 把
// minimatch 挪到子包下）会改路径串。这是有意的取舍——失败方向是响亮的红，
// 不是静默的绿。
//
// 用法：node scripts/audit-gate.mjs
// 更新期望值：确认新增条目确实该被豁免之后，改 EXPECTED_MUTED 并说明理由。

import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'

// 已知被豁免的确切路径。当前为空——曾经登记的 2 条都是 GHSA-mh99-v99m-4gvg
// （brace-expansion 超长展开 DoS），来自 eslint 与 openapi-typescript 两条不同
// 的 minimatch 链。2026-08-06 上游发了绕过该缓解的后续通告
// GHSA-rgw5-rvv9-x895，于是改成用 pnpm overrides 把三个 major 线的
// brace-expansion 直接顶到修复版（1.1.18 / 2.1.4 / 5.0.9），两条链一起消失，
// 通告级豁免也就不再需要——按门禁自己的规矩：能修就修，不要登记。
//
// 这里保持为空是有意义的状态，不是「还没填」：非空而 auditConfig 里没有豁免，
// 或反过来，都会被下面的一致性检查判红。
// gate 关心的严重度。护栏与门禁本体**必须共用这一个定义**——上一版护栏按它
// 过滤、本体却对任意严重度都红，于是「按严重度对齐」那次改动判定净零，而注释
// 里还写着「gate 本体只管 high+」，被同一段落下面五行的另一句话直接否定。
const GATE_LEVELS = new Set(['high', 'critical'])

const EXPECTED_MUTED = []

const pkg = JSON.parse(
  readFileSync(new URL('../../package.json', import.meta.url), 'utf8'),
)
const ignored = pkg.pnpm?.auditConfig?.ignoreGhsas ?? []

// 本脚本的**全部**输出走 stderr，一条不留在 stdout。
//
// 起因是「见上面的 ::notice::」这句交叉引用：上一版只把 notice 的打印块上移，
// 解决的是进程内写序；再上一版把 notice 改到 stderr，解决的是它与 error 的
// 跨流配对——但 `::group::` / `::endgroup::` 还留在 stdout，于是 notice 变得
// **可能落进默认折叠的 group 里**（改动前它在 stdout、必然位于 endgroup 之后，
// 是保证在外面的）。那不是消除跨流依赖，是把它换了个地方。
//
// runner 分别读两个管道再拼日志，任何跨流的先后关系都不受进程内顺序保证。
// 只有全部同流才谈得上「上面/下面」。
//
// 「stderr 上的 workflow command 照常被解析」这条依据是 **runner 实现**，不是
// 官方契约，必须说清楚：`actions/runner` 的 ScriptHandler 对 stdout 与 stderr
// 各建一个 OutputManager，但**共用同一个 ActionCommandManager**，而
// OutputManager 里没有任何区分流别的字段。反过来，GitHub 官方文档只写
// 「commands are sent to the runner over stdout」，全文不提 stderr。
// 也就是说这依赖的是实现细节——若哪天 runner 改成只解析 stdout，
// ::group:: / ::notice:: 会退化成普通文本行（不影响判定与退出码）。
console.error('::group::审计豁免清单')
if (ignored.length === 0) {
  console.error('（无豁免）')
} else {
  for (const id of ignored) console.error(`  - ${id}  https://github.com/advisories/${id}`)
}
console.error('期望登记的 (通告 id | 路径)：')
for (const p of EXPECTED_MUTED) console.error(`  ${p}`)
console.error('::endgroup::')

let raw
try {
  raw = execFileSync(
    'pnpm',
    // 不传 --audit-level：门槛只由上面的 GATE_LEVELS 决定，这里一份都不留。
    //
    // 上一版改成了 low，但那仍是第二个字面阈值，只是取了不会与 GATE_LEVELS
    // 冲突的极值——注释却写着「不留第二份」，说的比做的多。实测（pnpm 10.13）
    // 在 --json 模式下该 flag 对 advisories / actions / metadata / 退出码
    // 四者全无影响，所以直接去掉是安全的，而且顺带去掉一处隐性依赖：gate 的
    // 正确性不再取决于「advisories 不受 level 过滤」这条无法本机稳定复查的观测。
    ['audit', '--registry=https://registry.npmjs.org', '--json'],
    { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 },
  )
} catch (err) {
  // 有未豁免的漏洞时 pnpm 以非 0 退出，但 stdout 仍是完整 JSON。
  raw = err.stdout ?? ''
  if (!raw.trim()) {
    console.error('::error::pnpm audit 没有产出 JSON，无法判定——不能据此认为没有问题')
    console.error(String(err.stderr ?? err))
    process.exit(1)
  }
}

let report
try {
  report = JSON.parse(raw)
} catch {
  console.error('::error::pnpm audit 的输出不是合法 JSON，无法判定')
  console.error(raw.slice(0, 2000))
  process.exit(1)
}

// 护栏读的是 actions，不再是 metadata；这里校验的就是 actions 本身。
if (!Array.isArray(report.actions)) {
  console.error('::error::audit 输出缺少 actions 数组——护栏失去依据，视为失败')
  process.exit(1)
}

// 低于门槛的先打印出来再说。改动前它们会**错误地让 gate 变红**（且文案称之为
// 「高危」），若只是加上过滤就变成**安静地什么都不说**——那是把一种错换成另一
// 种。同一批修复里 codeql 的判定用的是「warning / note 打印但不拦」，这里保持
// 同一个标准。
const belowGate = Object.values(report.advisories ?? {}).filter(
  (a) => !GATE_LEVELS.has(a.severity),
)
if (belowGate.length > 0) {
  // 走 stderr 而不是 stdout。下面的错误信息里写着「见上面的 ::notice::」——
  // 只把这个块上移**只解决进程内写序**，两者仍分属两个流，而 Actions runner
  // 是分别读两个管道再拼日志，跨流先后不受进程内顺序保证。本机同终端跑恰好
  // 观测不到这一半，正是「实测覆盖不到断言全部含义」的那类。
  console.error(`::notice::${belowGate.length} 条低于 gate 门槛的通告（不拦，仅告知）`)
  for (const a of belowGate) {
    console.error(`  ${a.github_advisory_id ?? a.id} [${a.severity}] ${a.module_name}: ${a.title}`)
  }
}

let failed = false

// 护栏：被豁免的路径集合必须与期望**完全一致**（多一条少一条都红）。
if (ignored.length > 0) {
  // 注意：actions[].resolves[] 覆盖 pnpm 报出的**全部**漏洞，与是否被
  // ignoreGhsas 命中无关。今天两者恰好重合（唯一的通告正好被豁免），所以
  // 变量名叫 allVulns 而不是 muted——早先叫 actual 并在错误信息里写「未登记
  // 的被豁免路径」，会把一条**活的未修漏洞**指引成「登记进白名单」。
  // `actions[].resolves[]` **不受 --audit-level 过滤**（实测：--audit-level=high
  // 与 =critical 返回的条目数相同），而 gate 本体只管 high+。不过滤的话，任何
  // 一条 moderate/low 通告都会让护栏红，而它给出的两条出路对低危都是坏的——
  // 「修它」超出了 gate 的要求，「登记」则要往 ignoreGhsas 加一条**通告级永久
  // 放行**，为一条 low 付这个代价明显不对。
  //
  // 判别方法：`advisories` 带 severity 且**只**被 ignoreGhsas 过滤。
  //
  // 注意「--audit-level=low 与 =high 的 advisories 数量相同」这条观测是
  // **空证据**——本仓唯一的通告正好被豁免，两边都是 0，`0 == 0` 区分不了任何
  // 东西。决定性的对照是：临时清空 ignoreGhsas 后 `--audit-level=critical`
  // 仍然列出那条 high 通告（若被 level 过滤应返回 0）。于是：
  //   id 在 advisories 里 → 未被豁免，按它的真实 severity 决定要不要管；
  //   id 不在            → 正是被 ignoreGhsas 过滤掉的，必须登记。
  const severityByID = new Map(
    Object.values(report.advisories ?? {}).map((a) => [a.id, a.severity]),
  )
  const allVulns = [
    ...new Set(
      (report.actions ?? [])
        .flatMap((a) => a.resolves ?? [])
        .filter((r) => r.path)
        .filter((r) => !severityByID.has(r.id) || GATE_LEVELS.has(severityByID.get(r.id)))
        .map((r) => `${r.id}|${r.path}`),
    ),
  ].sort()
  const expected = [...EXPECTED_MUTED].sort()
  const added = allVulns.filter((p) => !expected.includes(p))
  const gone = expected.filter((p) => !allVulns.includes(p))

  if (added.length) {
    failed = true
    console.error(
      '::error::出现了未登记的漏洞条目。**先判断它是哪一种**：' +
        '(a) 新的活漏洞（high/critical）→ 修它，不要登记；' +
        '(b) 确认应豁免 → 同时更新 package.json 的 ignoreGhsas 与本文件的 EXPECTED_MUTED。' +
        `低于门槛**且未被豁免**的条目不会进这个列表${belowGate.length > 0 ? '（见上面的 ::notice::）' : ''}；` +
        '被 ignoreGhsas 豁免的条目无论严重度都会进——豁免之后 severity 不可见。' +
        'ignoreGhsas 是通告级过滤，登记错一条就等于永久放行一整类：',
    )
    for (const p of added) console.error(`  + ${p}`)
  }
  if (gone.length) {
    failed = true
    console.error(
      '::error::登记的条目已消失。这通常是好事（上游修了或依赖换了），' +
        '但必须确认后从 EXPECTED_MUTED 删除，否则豁免会一直挂着：',
    )
    for (const p of gone) console.error(`  - ${p}`)
  }
} else if (EXPECTED_MUTED.length > 0) {
  failed = true
  console.error('::error::EXPECTED_MUTED 非空但 auditConfig 里没有任何豁免——两者已脱节')
}

// 门禁本体：过滤后还剩、且达到 gate 门槛的，就是必须处理的。
//
// `advisories` **不受 --audit-level 过滤**（实测：临时清空 ignoreGhsas 后，
// --audit-level=critical 仍然列出那条 high）。所以门槛必须在这里自己判，
// 否则一条 moderate 也会红，且信息里还写着"高危"。
const advisories = Object.values(report.advisories ?? {}).filter((a) =>
  GATE_LEVELS.has(a.severity),
)
if (advisories.length > 0) {
  failed = true
  console.error(`::error::存在 ${advisories.length} 条未豁免的 high/critical 漏洞`)
  for (const a of advisories) {
    console.error(`  ${a.github_advisory_id ?? a.id} [${a.severity}] ${a.module_name}: ${a.title}`)
    for (const f of a.findings ?? []) {
      console.error(`    ${f.version} — ${(f.paths ?? []).slice(0, 3).join(', ')}`)
    }
  }
}

if (failed) process.exit(1)
console.error('✅ 无未豁免的 high/critical 漏洞；条目与登记一致')
