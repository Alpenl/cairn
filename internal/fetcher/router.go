package fetcher

// Router 按注册顺序匹配 Fetcher，先到先得。专用站点抓取器必须排在 basic 这种通配抓取器之前。
type Router struct {
	fetchers []Fetcher
}

// NewRouter 构造 Router 并按传入顺序注册所有 Fetcher。
func NewRouter(fetchers ...Fetcher) *Router {
	router := &Router{}
	for _, fetcher := range fetchers {
		router.Register(fetcher)
	}
	return router
}

// Register 追加一个 Fetcher 到路由列表尾部；传入 nil 时静默忽略以方便条件构造。
func (r *Router) Register(fetcher Fetcher) {
	if fetcher == nil {
		return
	}
	r.fetchers = append(r.fetchers, fetcher)
}

// Select 返回第一个 CanHandle 命中的 Fetcher；都不匹配返回 nil。
func (r *Router) Select(url string) Fetcher {
	for _, fetcher := range r.fetchers {
		if fetcher.CanHandle(url) {
			return fetcher
		}
	}
	return nil
}
