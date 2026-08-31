/**
 * 写操作之后的缓存失效入口（PF3）。
 *
 * 库内容的三份视图——链接列表、标签聚合、域名摘要——由同一批写事件改变：
 * 提交 / 删除链接、重新解析、保存或替换原文、链接转为网站。只失效其中一份，
 * 侧栏的数字与列表的内容就会在下一次校验之前互相矛盾。
 *
 * 原则是：**宁可多失效**。失效的代价
 * 是下一次读多一次网络往返（还有 in-flight 合并兜着），而漏失效的代价是用户
 * 明明操作成功了却看不到变化。
 */
import {
  ANNOTATED_LINKS_CACHE_KEY,
  DOMAIN_SUMMARIES_CACHE_PREFIX,
  FEED_ITEMS_CACHE_PREFIX,
  LINKS_CACHE_PREFIX,
  SUBSCRIPTIONS_CACHE_KEY,
  TAGS_CACHE_PREFIX,
  linkContentInvalidationPrefix,
  linkDetailCacheKey,
  linkTranslationsInvalidationPrefix,
} from './keys'
import { resourceStore } from './store'

export {  contentCacheKey } from './keys'

/**
 * 失效阅读库的全部聚合视图（链接列表 + 标签 + 域名摘要）。
 *
 * ⚠ `LINKS_CACHE_PREFIX` 是 `'GET /api/links'`，**没有尾随分隔符**，所以任何
 * 以它开头的键都会被这一行连坐打掉。正文与译文因此各自搬进了独立命名空间
 * （`CONTENT_CACHE_PREFIX`、`TRANSLATIONS_CACHE_PREFIX`）——两者都曾住在
 * `GET /api/links/...` 之下，于是「添加一条链接」就会清空全库的正文 / 译文。
 * 往这个前缀底下加新缓存键之前，先想清楚它该不该被库级写操作打掉。
 */
export function invalidateLibrary(): void {
  resourceStore.invalidate(LINKS_CACHE_PREFIX)
  resourceStore.invalidate(TAGS_CACHE_PREFIX)
  resourceStore.invalidate(DOMAIN_SUMMARIES_CACHE_PREFIX)
}

/**
 * 回收某条链接旧 revision 的已保存原文。
 *
 * 这个失效是**回收本机的孤儿条目**，不是正确性的最后防线。后端写正文时会递增
 * content_revision（`link_repo_content.go` 的 UpdateContentIfCurrent /
 * ReplaceContentIfCurrent 两条 UPDATE，守卫见 dbintegration 的
 * TestContentWritesBumpContentRevision），所以列表下次校验拿到新 revision 之后，
 * contentCacheKey 自然算出另一个键，旧内容读不到了。
 *
 * 那么为什么还留着？因为它打掉的是**本机旧键上那份已经没人会再读的正文**。
 * `LinkContentResponse` 自带 content_revision，MainView 的 onSaveContent /
 * onReplaceContent 当场把新代次回填进 Link，所以下一次读算出的就是新键——旧键
 * 上那条不会再被命中，但也不会自己消失，要靠这一行回收。
 *
 * 曾经这里写的是「响应不带 content_revision，所以有一段窗口靠这一行抹平」。那句
 * 话低估了问题：正文缓存那段窗口确实被抹平了，但同一段窗口里，划线 envelope 用
 * 的也是旧代次，而 invalidateLink 对它无能为力——列表刷新一到就被判失配丢掉，
 * 用户数据没了。真正的修法是让响应带上代次，已经这么做了；这个入口退回它本来
 * 的职责：回收正文孤儿条目。译文也按 revision 分区，正文代次前进时由新 key
 * 自然加载，不能先失效旧 key，否则 React 提交新 revision 前会多请求旧 generation。
 */
export function invalidateLinkContent(linkId: string): void {
  resourceStore.invalidate(linkContentInvalidationPrefix(linkId))
}

/** 清除一条链接的 point projection，并让 mounted annotated 视图重新枚举/点读。 */
export function invalidateLinkProjection(linkId: string): void {
  resourceStore.invalidate(linkDetailCacheKey(linkId))
  resourceStore.invalidate(ANNOTATED_LINKS_CACHE_KEY)
}

/** 失效某条链接的详情、译文 generation、正文，并唤醒 annotated 投影。 */
export function invalidateLink(linkId: string): void {
  // 缓存键都走各自的 owner，不照抄字面量：抄一份的话，那边改名这边会
  // 静默断掉——而「静默断掉」在失效这件事上的症状是用户看着过期数据，不报错。
  //
  // 正文与译文前缀都带尾随分隔符（`/` 与 `?`），于是前缀匹配退化成**精确**匹配：
  // encodeURIComponent 从不产出 `/` 或 `?` 且是单射，所以 `enc(a)+sep` 成为
  // `enc(b)+sep+...` 的前缀当且仅当 a === b。不带分隔符时 invalidateLink('L1')
  // 会顺带打掉 L12、L1a——线上 id 是定长 UUID 互不为前缀所以打不中，但那是靠
  // 调用方数据形状兜底的正确性。
  invalidateLinkProjection(linkId)
  resourceStore.invalidate(linkTranslationsInvalidationPrefix(linkId))
  invalidateLinkContent(linkId)
}

/**
 * 失效订阅侧的全部缓存（订阅导航 + 所有筛选下的条目列表）。
 *
 * 必须按**前缀**打掉条目列表：每个筛选（全部 / 未读 / 收藏 / 某个订阅源 /
 * 某个文件夹）是一个独立的缓存键，而取消订阅、批量已读、导入 OPML 这类操作
 * 会同时改变其中好几个。只重取当前那个视图的话，切到别的筛选会看到过期的
 * 未读数。
 */
export function invalidateFeeds(): void {
  resourceStore.invalidate(SUBSCRIPTIONS_CACHE_KEY)
  resourceStore.invalidate(FEED_ITEMS_CACHE_PREFIX)
}
