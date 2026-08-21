package dbintegration

import (
	"webtag/internal/repository"
	"webtag/internal/service"
)

func postgresReaderStores(store *repository.PGXReaderVNextRepository) service.ReaderStores {
	return service.ReaderStores{
		Thoughts: store,
		Notes:    store,
		Inbox:    store,
		Todos:    store,
		Library:  store,
		Hosts:    store,
	}
}
