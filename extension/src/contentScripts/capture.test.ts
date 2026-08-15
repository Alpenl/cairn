/**
 * capture.test.ts — capturePageContent 抓取函数单元测试。
 *
 * capturePageContent 是注入目标页面执行的纯函数，仅依赖 DOM / window，
 * 在 jsdom 环境下可直接调用。
 *
 * jsdom 中 innerText 等价于 textContent。
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { capturePageContent } from './capture'

/** 重置文档到干净状态。 */
function resetDocument(): void {
  document.head.innerHTML = ''
  document.body.innerHTML = ''
  document.documentElement.lang = ''
  document.title = ''
}

describe('capturePageContent — 基础抓取', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  it('返回包含全部 RawCapture 字段的对象', () => {
    document.title = '测试页面'
    document.body.innerHTML = '<p>正文内容</p>'

    const result = capturePageContent()

    expect(result).toHaveProperty('url')
    expect(result).toHaveProperty('title')
    expect(result).toHaveProperty('text')
    expect(result).toHaveProperty('html')
    expect(result).toHaveProperty('imageUrls')
    expect(result).toHaveProperty('metadata')
    expect(Array.isArray(result.imageUrls)).toBe(true)
  })

  it('抓取页面标题与 URL', () => {
    document.title = 'WebTag 采集'
    const result = capturePageContent()
    expect(result.title).toBe('WebTag 采集')
    expect(result.url).toBe(document.location.href)
  })

  it('保留脱敏正文结构但不枚举页面图片', () => {
    document.body.innerHTML = `
      <article><h1>动态标题</h1><p>动态插入的正文</p><script>alert('x')</script></article>
      <img src="https://example.com/content.png" />
    `
    const result = capturePageContent()

    expect(result.html).toContain('<article>')
    expect(result.html).toContain('<h1>动态标题</h1>')
    expect(result.html).toContain('<p>动态插入的正文</p>')
    expect(result.html).not.toContain('<script')
    expect(result.imageUrls).toEqual([])
  })

  it('结构快照剥离事件属性、表单值和危险链接', () => {
    document.body.innerHTML = `
			<article onclick="steal()">
				<p style="color:red">安全正文</p>
				<input value="private@example.com" />
				<a href="javascript:alert(1)" data-token="secret">危险</a>
				<a href="/docs">文档</a>
			</article>
		`
    const result = capturePageContent()
    expect(result.html).not.toContain('onclick')
    expect(result.html).not.toContain('style=')
    expect(result.html).not.toContain('private@example.com')
    expect(result.html).not.toContain('javascript:')
    expect(result.html).not.toContain('data-token')
    expect(result.html).toContain('href="http://localhost:3000/docs"')
  })

  it('结构快照不把 CSS 隐藏文本重新暴露', () => {
    const style = document.createElement('style')
    style.textContent = '.secret-by-class { display: none }'
    document.head.appendChild(style)
    document.body.innerHTML = `
      <article>
        <p>公开正文</p>
        <div style="display:none">inline-hidden-secret</div>
        <div class="secret-by-class">class-hidden-secret</div>
        <div style="visibility:hidden">visibility-hidden-secret</div>
      </article>
    `

    const result = capturePageContent()

    expect(result.html).toContain('公开正文')
    expect(result.html).not.toContain('inline-hidden-secret')
    expect(result.html).not.toContain('class-hidden-secret')
    expect(result.html).not.toContain('visibility-hidden-secret')
  })
})

describe('capturePageContent — 正文文本提取', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  it('优先从 <article> 容器提取正文', () => {
    document.body.innerHTML = `
      <nav>导航不应出现</nav>
      <article>这是文章正文</article>
      <footer>页脚不应出现</footer>
    `
    const result = capturePageContent()
    expect(result.text).toContain('这是文章正文')
    expect(result.text).not.toContain('导航不应出现')
    expect(result.text).not.toContain('页脚不应出现')
  })

  it('无已知正文容器时回退到 body 并剔除噪声标签', () => {
    document.body.innerHTML = `
      <script>console.log('脚本噪声')</script>
      <div>页面主体文字</div>
      <style>.x{color:red}</style>
    `
    const result = capturePageContent()
    expect(result.text).toContain('页面主体文字')
    expect(result.text).not.toContain('脚本噪声')
    expect(result.text).not.toContain('color:red')
  })

  it('折叠多余空白：连续空行压成单换行', () => {
    document.body.innerHTML = '<article>第一行\n\n\n\n第二行</article>'
    const result = capturePageContent()
    // 不应出现连续多个换行。
    expect(result.text).not.toMatch(/\n\n/)
  })
})

describe('capturePageContent — 元数据', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  it('始终包含 capture_source 与 captured_at', () => {
    const result = capturePageContent()
    expect(result.metadata.capture_source).toBe('browser_extension')
    expect(typeof result.metadata.captured_at).toBe('string')
  })

  it('抓取 meta description 与 og:title', () => {
    const desc = document.createElement('meta')
    desc.setAttribute('name', 'description')
    desc.setAttribute('content', '页面描述文本')
    document.head.appendChild(desc)

    const ogTitle = document.createElement('meta')
    ogTitle.setAttribute('property', 'og:title')
    ogTitle.setAttribute('content', 'OG 标题')
    document.head.appendChild(ogTitle)

    const result = capturePageContent()
    expect(result.metadata.description).toBe('页面描述文本')
    expect(result.metadata.og_title).toBe('OG 标题')
  })

  it('仅采集有界的无正文分类信号', () => {
    document.head.innerHTML = `
      <meta property="og:type" content="website" />
      <meta name="application-name" content="Workspace" />
      <link rel="manifest" href="/manifest.webmanifest" />
      <script type="application/ld+json">{"@context":"https://schema.org","@type":"WebApplication","description":"不应进入信号"}</script>
    `
    document.body.innerHTML = `<nav>${'<a>nav</a>'.repeat(8)}</nav><main>${'<p>正文</p>'.repeat(4)}</main>`

    const result = capturePageContent()

    expect(result.metadata.og_type).toBe('website')
    expect(result.metadata.has_application_name).toBe('true')
    expect(result.metadata.has_web_app_manifest).toBe('true')
    expect(result.metadata.navigation_dominant).toBe('true')
    expect(result.metadata.prose_dominant).toBe('true')
    expect(result.metadata.jsonld_types).toBe('WebApplication')
    expect(result.metadata.jsonld_types).not.toContain('不应进入信号')
  })

  it('抓取 documentElement.lang', () => {
    document.documentElement.lang = 'zh-CN'
    const result = capturePageContent()
    expect(result.metadata.lang).toBe('zh-CN')
  })
})

describe('capturePageContent — 正文脱敏', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  it('剥离敏感数据在克隆上进行，不影响真实页面 DOM', () => {
    document.body.innerHTML = `
      <input id="pwd" type="password" value="原始密码" />
    `
    capturePageContent()
    // 真实页面的 password input 仍在、value 未被改动。
    const realInput = document.getElementById('pwd') as HTMLInputElement
    expect(realInput).not.toBeNull()
    expect(realInput.value).toBe('原始密码')
  })
})

describe('capturePageContent — 正文不包含表单数据', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  it('<textarea> 草稿不出现在正文', () => {
    // 放进 <article> 容器，命中正文容器探测分支（text 从脱敏克隆取）。
    document.body.innerHTML = `
      <article>
        <p>文章正文段落</p>
        <textarea name="draft">用户尚未发布的私密草稿</textarea>
      </article>
    `
    const result = capturePageContent()
    expect(result.text).toContain('文章正文段落')
    expect(result.text).not.toContain('用户尚未发布的私密草稿')
  })

  it('回退到 body 时，<textarea> 草稿也不出现在正文', () => {
    // 无已知正文容器 → 走 body 回退分支，验证该分支也经脱敏克隆取文。
    document.body.innerHTML = `
      <div>页面普通文字</div>
      <textarea name="draft">body 回退路径下的私密草稿</textarea>
    `
    const result = capturePageContent()
    expect(result.text).toContain('页面普通文字')
    expect(result.text).not.toContain('body 回退路径下的私密草稿')
  })

  it('已填写 input 的 value 不经 text 路径外泄', () => {
    // input 的 value 通常不进 innerText，但仍放在 article 内覆盖脱敏克隆路径。
    document.body.innerHTML = `
      <article>
        <p>表单文章</p>
        <input type="text" name="email" value="leaked@private.com" />
      </article>
    `
    const result = capturePageContent()
    expect(result.text).not.toContain('leaked@private.com')
  })

  it('password 输入框内容不经 text 路径外泄', () => {
    document.body.innerHTML = `
      <article>
        <p>登录页正文</p>
        <input type="password" name="pwd" value="text路径机密密码" />
      </article>
    `
    const result = capturePageContent()
    expect(result.text).not.toContain('text路径机密密码')
  })

  it('清除 contenteditable 与 ARIA textbox 草稿但保留公开正文', () => {
    document.body.innerHTML = `
      <article>
        <p>公开正文应保留</p>
        <div contenteditable="true">contenteditable-private-draft</div>
        <div role="textbox">aria-textbox-private-draft</div>
      </article>
    `

    const result = capturePageContent()

    expect(result.text).toContain('公开正文应保留')
    expect(result.html).toContain('公开正文应保留')
    for (const draft of [
      'contenteditable-private-draft',
      'aria-textbox-private-draft',
    ]) {
      expect(result.text).not.toContain(draft)
      expect(result.html).not.toContain(draft)
    }
  })

  it('body fallback 清除继承、plaintext-only 与 ARIA textbox 草稿', () => {
    document.body.innerHTML = `
      <div>body-public-copy</div>
      <section contenteditable="true">
        inherited-parent-draft
        <span>inherited-child-draft</span>
      </section>
      <div contenteditable="plaintext-only">plaintext-private-draft</div>
      <div role="textbox"><span>nested-aria-private-draft</span></div>
    `

    const result = capturePageContent()

    expect(result.text).toContain('body-public-copy')
    expect(result.html).toContain('body-public-copy')
    for (const draft of [
      'inherited-parent-draft',
      'inherited-child-draft',
      'plaintext-private-draft',
      'nested-aria-private-draft',
    ]) {
      expect(result.text).not.toContain(draft)
      expect(result.html).not.toContain(draft)
    }
  })

  it('显式 contenteditable=false 边界保留公开文本并允许内部重新启用编辑', () => {
    document.body.innerHTML = `
      <article contenteditable="true">
        outer-private-draft
        <div contenteditable="false">
          false-boundary-public-copy
          <span>inherited-public-copy</span>
          <span contenteditable="true">reenabled-private-draft</span>
        </div>
      </article>
    `
    const article = document.querySelector<HTMLElement>('article')
    expect(article).not.toBeNull()
    Object.defineProperty(article, 'innerText', {
      configurable: true,
      value: article?.textContent ?? '',
    })

    const result = capturePageContent()

    expect(result.text).toContain('false-boundary-public-copy')
    expect(result.text).toContain('inherited-public-copy')
    expect(result.html).toContain('false-boundary-public-copy')
    expect(result.text).not.toContain('outer-private-draft')
    expect(result.text).not.toContain('reenabled-private-draft')
    expect(result.html).not.toContain('outer-private-draft')
    expect(result.html).not.toContain('reenabled-private-draft')
  })

  it('正文容器继承外部 contenteditable=true 时清除编辑草稿', () => {
    document.body.innerHTML = `
      <section contenteditable="true">
        <article>ancestor-inherited-private-draft</article>
      </section>
    `
    const article = document.querySelector<HTMLElement>('article')
    expect(article).not.toBeNull()
    Object.defineProperty(article, 'innerText', {
      configurable: true,
      value: article?.textContent ?? '',
    })

    const result = capturePageContent()

    expect(result.text).not.toContain('ancestor-inherited-private-draft')
    expect(result.html).not.toContain('ancestor-inherited-private-draft')
    expect(article?.textContent).toBe('ancestor-inherited-private-draft')
  })

  it('正文容器外部的 contenteditable=false 边界阻止继承清除', () => {
    document.body.innerHTML = `
      <div contenteditable="true">
        <section contenteditable="false">
          <article>false-ancestor-public-copy</article>
        </section>
      </div>
    `
    const article = document.querySelector<HTMLElement>('article')
    expect(article).not.toBeNull()
    Object.defineProperty(article, 'innerText', {
      configurable: true,
      value: article?.textContent ?? '',
    })

    const result = capturePageContent()

    expect(result.text).toContain('false-ancestor-public-copy')
    expect(result.html).toContain('false-ancestor-public-copy')
    expect(article?.textContent).toBe('false-ancestor-public-copy')
  })

  it.each([
    ['contenteditable root', 'contenteditable="true"'],
    ['ARIA textbox root', 'role="textbox"'],
  ])('正文容器本身是 %s 时只清 clone 且不修改真实 DOM', (_name, attr) => {
    document.body.innerHTML = `<article ${attr}>root-editor-private-draft</article>`
    const article = document.querySelector<HTMLElement>('article')
    expect(article).not.toBeNull()
    Object.defineProperty(article, 'innerText', {
      configurable: true,
      value: article?.textContent ?? '',
    })

    const result = capturePageContent()

    expect(result.text).not.toContain('root-editor-private-draft')
    expect(result.html).not.toContain('root-editor-private-draft')
    expect(article?.textContent).toBe('root-editor-private-draft')
  })

  it.each(['article', 'main'] as const)(
    '%s 正文容器剔除可执行与隐藏内容，同时保留可见正文',
    (containerTag) => {
      document.body.innerHTML = `
        <${containerTag}>
          <p>public article body</p>
          <script>script-secret-token</script>
          <style>.style-secret-marker { color: red; }</style>
          <template>template-secret-state</template>
          <iframe>iframe-secret-fallback</iframe>
          <div hidden>hidden-secret-value</div>
          <div aria-hidden="true">aria-hidden-secret-value</div>
        </${containerTag}>
      `
      // jsdom does not calculate layout-backed innerText. Give the live node a
      // visible value so selector probing takes the article/main branch; the
      // cloned node still exercises the production textContent fallback.
      const container = document.querySelector<HTMLElement>(containerTag)
      expect(container).not.toBeNull()
      Object.defineProperty(container, 'innerText', {
        configurable: true,
        value: container?.textContent ?? '',
      })

      const result = capturePageContent()

      expect(result.text).toContain('public article body')
      for (const secret of [
        'script-secret-token',
        'style-secret-marker',
        'template-secret-state',
        'iframe-secret-fallback',
        'hidden-secret-value',
        'aria-hidden-secret-value',
      ]) {
        expect(result.text).not.toContain(secret)
      }
    },
  )
})

describe('capturePageContent — 大小封顶', () => {
  beforeEach(resetDocument)
  afterEach(resetDocument)

  /** 内容脚本通过扩展消息通道返回的正文字符上限。 */
  const MAX_CHARS = 512 * 1024

  it('超大正文 innerText 被截到 MAX_CHARS 字符以内', () => {
    // 构造一段远超上限的正文。
    const huge = 'A'.repeat(MAX_CHARS + 10000)
    document.body.innerHTML = `<article>${huge}</article>`
    const result = capturePageContent()
    expect(result.text.length).toBeLessThanOrEqual(MAX_CHARS)
    expect(result.html.length).toBeLessThanOrEqual(MAX_CHARS)
  })

  it('正常大小页面不受封顶影响，内容完整', () => {
    document.body.innerHTML = '<article>普通长度的正文</article>'
    const result = capturePageContent()
    expect(result.text).toContain('普通长度的正文')
  })
})
