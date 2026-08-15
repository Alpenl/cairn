/**
 * @module contentScripts/capture
 * 当前页采集脚本 —— 提取标签页「已渲染」的正文与元数据。
 *
 * 注入机制：**chrome.scripting.executeScript 按需注入**，不是静态 content_scripts。
 * 选择理由：
 *   - 采集只在用户主动点击「采集到 WebTag」或右键菜单时发生，无需在每个页面常驻。
 *     静态注入会让抓取代码在所有页面加载时执行，纯属浪费。
 *   - NaiveTab 既有的静态 content script（contentScripts/index.ts）职责是「响应键盘
 *     快捷键」，与采集无关。把采集塞进同一 IIFE bundle 会污染其单一职责、增大体积。
 *   - manifest 已有 scripting 权限 + 全站 host_permissions，executeScript
 *     可直接对活动标签注入函数，无需改 manifest。
 *
 * 抓取逻辑作为「可序列化的纯函数」`capturePageContent` 导出。background 通过
 * `chrome.scripting.executeScript({ target, func: capturePageContent })` 注入执行，
 * 函数在目标页面的隔离世界中运行、返回结果被结构化克隆回 background。
 * 因此本函数**不能引用模块作用域外的任何变量 / import**——所有依赖（常量、
 * 子函数）必须内联在函数体内。这是 executeScript func 注入的硬约束。
 *
 * 返回值 RawCapture 是「未归一化」的原始抓取结果；归一化为后端
 * `IngestSource`（kind=browser_capture）由 background/captureHandler.ts 完成
 * （background 上下文才有 src/api 类型与裁剪逻辑）。
 *
 * 消费者：src/background/captureHandler.ts。
 */

/**
 * capturePageContent 在目标页面执行后返回的原始抓取结果。
 *
 * 字段与后端 `IngestSource`（browser_capture）保持同形。html 是与 text
 * 来自同一脱敏正文克隆的结构快照，不是整页 DOM；imageUrls 仍固定为空，
 * 不默认触发视觉分析。精确字节裁剪由 background 归一化完成。
 */
export interface RawCapture {
  /** 页面当前 URL（document.location.href，已渲染后的真实地址）。 */
  url: string
  /** 页面标题（document.title）。 */
  title: string
  /** 提取出的正文纯文本。 */
  text: string
  /** 脱敏后的正文 HTML 结构快照。 */
  html: string
  /** 协议兼容字段；普通采集固定为空。 */
  imageUrls: string[]
  /** 附加元数据：语言、字符集、描述、抓取时间等。 */
  metadata: Record<string, string>
}

/**
 * 在「目标页面上下文」中执行的抓取函数。
 *
 * ⚠️ 注入约束：本函数体被 chrome.scripting.executeScript 序列化后在目标页面执行，
 * 不能引用任何外部作用域的标识符（import、模块常量、其它函数）。所有逻辑必须自包含。
 *
 * 安全与健壮性（本次加固）：
 *   - **敏感数据剥离**：正文在 DOM 克隆上剥离表单数据——整体移除
 *     password / hidden 类型的 input，并清空其余
 *     input/textarea/select 的 value/checked。text 路径从「脱敏后的克隆」
 *     取 innerText（而非直接读 live mainEl 或未脱敏克隆），杜绝
 *     `<textarea>` 草稿、已填写表单的渲染文本经 RawCapture.text 外泄。
 *   - **大小封顶**：正文 innerText 在返回前截断，避免通过扩展消息通道
 *     传递无界字符串。
 *   - **不抛错**：DOM 克隆包在 try/catch 内，失败时返回
 *     部分结果而非抛出，让采集流程不因单页异常而整体失败。
 *
 * @returns 已渲染页面的原始抓取结果（可能为「部分结果」）。
 */
export function capturePageContent(): RawCapture {
  // ── 内联常量 ────────────────────────────────────────────
  // 优先尝试的正文容器选择器，按「越具体越靠前」排序。
  const MAIN_CONTENT_SELECTORS = [
    'article',
    'main',
    '[role="main"]',
    '#content',
    '#main',
    '.post-content',
    '.article-content',
    '.markdown-body',
  ]
  // 抓取正文时跳过的噪声标签（脚本、样式、导航、页脚等）。
  const NOISE_TAGS = new Set([
    'SCRIPT',
    'STYLE',
    'NOSCRIPT',
    'NAV',
    'HEADER',
    'FOOTER',
    'ASIDE',
    'TEMPLATE',
    'IFRAME',
    'OBJECT',
    'EMBED',
    'SVG',
    'MATH',
    'CANVAS',
    'AUDIO',
    'VIDEO',
  ])
  // 正文字符上限；background 还会按 UTF-8 字节限制做精确裁剪。
  const MAX_CHARS = 512 * 1024

  // ── 工具函数（内联） ────────────────────────────────────
  /** 把字符串截到 MAX_CHARS 字符以内，杜绝无界巨串。 */
  const capChars = (value: string): string =>
    value.length > MAX_CHARS ? value.slice(0, MAX_CHARS) : value

  /**
   * 在 **传入的元素（应为克隆，非 live DOM）** 上原地剥离敏感表单数据。
   *
   * 处理项：
   *   - 整体移除 input[type=password] / input[type=hidden]——密码与隐藏字段
   *     （CSRF token、会话标识、隐藏的用户信息等）绝不能外泄。
   *   - 其余 input：移除 value 属性、清空 .value，并对 checkbox/radio
   *     去掉 checked——避免用户已填写的内容被采集。
   *   - textarea：移除 value 属性、清空文本子节点与 .value——textarea 的
   *     已输入内容既体现在 .value、也体现在渲染文本（innerText），两者都清。
   *   - select：去掉所有 option 的 selected——避免泄露用户的选择。
   *
   * ⚠️ 调用方必须传克隆——本函数原地改 DOM，确保 RawCapture.text
   * 不含表单草稿。
   *
   * @param root 已克隆的根元素（绝不能传 live DOM）
   */
  const stripSensitiveForms = (root: HTMLElement): void => {
    // 1. 整体移除 password / hidden 输入框。
    root
      .querySelectorAll('input[type="password"], input[type="hidden"]')
      .forEach((node) => {
        node.parentNode?.removeChild(node)
      })
    // 2. 清空其余 input 的 value / checked。
    root.querySelectorAll('input').forEach((node) => {
      node.removeAttribute('value')
      node.removeAttribute('checked')
      const input = node as HTMLInputElement
      input.value = ''
      input.checked = false
    })
    // 3. 清空 textarea 的内容（value 属性 + 文本子节点 + .value）。
    //    textContent 清空确保 innerText 渲染文本里也不残留草稿。
    root.querySelectorAll('textarea').forEach((node) => {
      node.removeAttribute('value')
      node.textContent = ''
      ;(node as HTMLTextAreaElement).value = ''
    })
    // 4. 清空 select 的选中状态（去掉所有 option 的 selected）。
    root.querySelectorAll('select option').forEach((node) => {
      node.removeAttribute('selected')
      ;(node as HTMLOptionElement).selected = false
    })
  }

  /**
   * Clear user-authored editor text from the cloned tree before editor marker
   * attributes are removed. contenteditable is inherited, except that an
   * explicit false value creates a non-editable boundary. ARIA textboxes are
   * treated as complete editor surfaces because role is not inherited.
   */
  const stripEditableDrafts = (
    root: HTMLElement,
    liveRoot: HTMLElement,
  ): void => {
    const visit = (node: HTMLElement, inheritedEditable: boolean): void => {
      const rawEditable = node.getAttribute('contenteditable')
      const editableValue = rawEditable?.trim().toLowerCase()
      const editable =
        rawEditable === null ||
        (editableValue !== '' &&
          editableValue !== 'true' &&
          editableValue !== 'plaintext-only' &&
          editableValue !== 'false')
          ? inheritedEditable
          : editableValue !== 'false'

      if (node.getAttribute('role')?.trim().toLowerCase() === 'textbox') {
        node.textContent = ''
        return
      }

      for (const child of Array.from(node.childNodes)) {
        if (child.nodeType === Node.TEXT_NODE) {
          if (editable) child.parentNode?.removeChild(child)
          continue
        }
        if (child instanceof HTMLElement) visit(child, editable)
      }
    }

    const ancestors: HTMLElement[] = []
    for (
      let ancestor = liveRoot.parentElement;
      ancestor;
      ancestor = ancestor.parentElement
    ) {
      ancestors.push(ancestor)
    }
    let inheritedEditable = false
    for (const ancestor of ancestors.reverse()) {
      const raw = ancestor.getAttribute('contenteditable')
      const value = raw?.trim().toLowerCase()
      if (raw === null) continue
      if (value === '' || value === 'true' || value === 'plaintext-only') {
        inheritedEditable = true
      } else if (value === 'false') {
        inheritedEditable = false
      }
    }

    visit(root, inheritedEditable)
  }

  /** 只保留阅读结构需要的属性，并把链接/图片 URL 限制为安全协议。 */
  const stripUnsafeAttributes = (root: HTMLElement): void => {
    const nodes = [root, ...Array.from(root.querySelectorAll<HTMLElement>('*'))]
    for (const node of nodes) {
      for (const attr of Array.from(node.attributes)) {
        const name = attr.name.toLowerCase()
        const tag = node.tagName
        const keepTextAttribute =
          (tag === 'IMG' && (name === 'alt' || name === 'title')) ||
          (tag === 'A' && name === 'title') ||
          (tag === 'OL' && name === 'start') ||
          ((tag === 'TD' || tag === 'TH') &&
            (name === 'colspan' || name === 'rowspan')) ||
          (tag === 'CODE' &&
            name === 'class' &&
            /^language-[A-Za-z0-9_+.-]{1,64}$/.test(attr.value))
        if (keepTextAttribute) continue

        if (
          (tag === 'A' && name === 'href') ||
          (tag === 'IMG' && name === 'src')
        ) {
          try {
            const parsed = new URL(attr.value, document.baseURI)
            const allowed =
              tag === 'IMG'
                ? parsed.protocol === 'http:' || parsed.protocol === 'https:'
                : ['http:', 'https:', 'mailto:', 'tel:'].includes(
                    parsed.protocol,
                  )
            if (allowed) {
              parsed.username = ''
              parsed.password = ''
              node.setAttribute(name, parsed.toString())
              continue
            }
          } catch {
            // Invalid URLs are removed below.
          }
        }
        node.removeAttribute(attr.name)
      }
    }
  }

  /** 在正文克隆上按真实 DOM 的可见性删除节点，并清理可执行内容与表单状态。 */
  const sanitizeContentClone = (
    root: HTMLElement,
    liveRoot: HTMLElement,
  ): void => {
    const cloneNodes = [
      root,
      ...Array.from(root.querySelectorAll<HTMLElement>('*')),
    ]
    const liveNodes = [
      liveRoot,
      ...Array.from(liveRoot.querySelectorAll<HTMLElement>('*')),
    ]
    cloneNodes.forEach((node, index) => {
      const liveNode = liveNodes[index]
      const ariaHidden = liveNode?.getAttribute('aria-hidden')
      let hiddenByCSS = false
      try {
        if (liveNode) {
          const computed = window.getComputedStyle(liveNode)
          hiddenByCSS =
            computed.display === 'none' ||
            computed.visibility === 'hidden' ||
            computed.contentVisibility === 'hidden'
        }
      } catch {
        // Some restricted documents do not expose computed styles. The
        // explicit hidden/aria checks below still provide a safe fallback.
      }
      if (
        NOISE_TAGS.has(node.tagName) ||
        liveNode?.hidden ||
        hiddenByCSS ||
        ariaHidden?.toLowerCase() === 'true'
      ) {
        node.parentNode?.removeChild(node)
      }
    })
    stripSensitiveForms(root)
    stripEditableDrafts(root, liveRoot)
    stripUnsafeAttributes(root)
  }

  // ── 1. 正文文本提取（脱敏）──────────────────────────────
  // 优先在已知正文容器中取正文，否则回退到 body。innerText 反映渲染后
  // 可见文本，自动跳过隐藏节点，比 textContent 更贴近用户实际看到的内容。
  //
  // ⚠️ 安全：正文 **必须从脱敏克隆** 提取，绝不直接读 live mainEl /
  // 未脱敏克隆。否则 <textarea> 草稿、已填写表单的渲染文本会经
  // RawCapture.text 外泄，因此正文统一经 stripSensitiveForms 剥离。
  //
  // 容器探测仍用 live DOM 上的 innerText（判断「是否有正文」即可，
  // 不外泄）；真正取文用脱敏后的克隆。
  let mainEl: HTMLElement | null = null
  for (const selector of MAIN_CONTENT_SELECTORS) {
    const candidate = document.querySelector<HTMLElement>(selector)
    if (candidate && (candidate.innerText || '').trim().length > 0) {
      mainEl = candidate
      break
    }
  }

  let text = ''
  let html = ''
  try {
    if (mainEl) {
      // 克隆正文容器 → 剥离表单数据 → 取脱敏后的 innerText。
      const clone = mainEl.cloneNode(true) as HTMLElement
      sanitizeContentClone(clone, mainEl)
      text = clone.innerText || clone.textContent || ''
      html = clone.outerHTML
    } else if (document.body) {
      // 回退：克隆 body，剔除噪声标签 + 剥离表单数据后取 innerText。
      const clone = document.body.cloneNode(true) as HTMLElement
      sanitizeContentClone(clone, document.body)
      text = clone.innerText || clone.textContent || ''
      html = clone.outerHTML
    }
    // 折叠多余空白：连续空行压成单个换行，行尾空白去除。
    text = text
      .split('\n')
      .map((line) => line.replace(/\s+/g, ' ').trim())
      .filter((line) => line.length > 0)
      .join('\n')
  } catch {
    // 正文提取异常：保留已取到的部分（可能为空），不中断采集。
  }
  // 字符封顶：超大正文在此截断，杜绝无界字符串。
  text = capChars(text)
  html = capChars(html)

  // ── 2. 元数据 ───────────────────────────────────────────
  // 普通命名 meta（如 <meta name="description">）取 content。
  const metaByName = (n: string): string =>
    document
      .querySelector(`meta[name="${n}"]`)
      ?.getAttribute('content')
      ?.trim() || ''
  // OpenGraph meta 一律是 <meta property="og:...">，按 property 查询。
  const metaByProp = (p: string): string =>
    document
      .querySelector(`meta[property="${p}"]`)
      ?.getAttribute('content')
      ?.trim() || ''
  const metadata: Record<string, string> = {
    capture_source: 'browser_extension',
    captured_at: new Date().toISOString(),
  }
  const lang = document.documentElement?.lang?.trim()
  if (lang) metadata.lang = lang
  if (document.characterSet) metadata.charset = document.characterSet
  // description 优先取 <meta name="description">，缺失时回退 og:description。
  const description = metaByName('description') || metaByProp('og:description')
  if (description) metadata.description = description
  const ogTitle = metaByProp('og:title')
  if (ogTitle) metadata.og_title = ogTitle
  const ogSiteName = metaByProp('og:site_name')
  if (ogSiteName) metadata.site_name = ogSiteName

  // Classification signals deliberately contain only bounded structural
  // facts. Never retain JSON-LD script text, element text, or form values.
  const ogType = metaByProp('og:type')
  if (ogType) metadata.og_type = ogType.toLowerCase()
  if (document.querySelector('link[rel="manifest"]'))
    metadata.has_web_app_manifest = 'true'
  if (metaByName('application-name')) metadata.has_application_name = 'true'
  if (
    document.querySelector('meta[name="author"], [rel="author"]') &&
    document.querySelector(
      'meta[property="article:published_time"], time[datetime]',
    )
  ) {
    metadata.has_author_and_published = 'true'
  }
  if (
    description &&
    (document.querySelector('main, [role="main"], article') ||
      document.querySelectorAll('nav a').length > 4)
  ) {
    metadata.has_site_description = 'true'
  }
  if (document.querySelectorAll('nav a').length >= 8)
    metadata.navigation_dominant = 'true'
  if (
    document.querySelectorAll('button, input[type="submit"], [role="button"]')
      .length >= 4
  ) {
    metadata.interactive_dominant = 'true'
  }
  if (
    document.querySelectorAll('article p, main p, [role="main"] p').length >= 4
  )
    metadata.prose_dominant = 'true'

  const jsonLDTypes = new Set<string>()
  const addJSONLDType = (value: unknown): void => {
    if (typeof value === 'string' && value.length <= 64)
      jsonLDTypes.add(value.trim())
    if (Array.isArray(value)) value.forEach(addJSONLDType)
  }
  document
    .querySelectorAll('script[type="application/ld+json"]')
    .forEach((script, index) => {
      if (index >= 20 || jsonLDTypes.size >= 20) return
      try {
        const data = JSON.parse(script.textContent || '') as unknown
        const visit = (value: unknown): void => {
          if (!value || typeof value !== 'object') return
          if (Array.isArray(value)) {
            value.slice(0, 20).forEach(visit)
            return
          }
          const record = value as Record<string, unknown>
          addJSONLDType(record['@type'])
          if (Array.isArray(record['@graph']))
            record['@graph'].slice(0, 20).forEach(visit)
        }
        visit(data)
      } catch {
        // Invalid JSON-LD is common on production pages and is not a capture error.
      }
    })
  if (jsonLDTypes.size > 0)
    metadata.jsonld_types = Array.from(jsonLDTypes).slice(0, 20).join(',')

  return {
    url: document.location.href,
    title: document.title || '',
    text,
    html,
    imageUrls: [],
    metadata,
  }
}
