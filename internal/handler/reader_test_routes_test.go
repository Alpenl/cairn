package handler

import "webtag/internal/service"

func readerTestRoutes(candidate any) ReaderRoutes {
	if applications, ok := candidate.(*service.ReaderApplications); ok {
		return ReaderRoutes{
			Thoughts: NewReaderThoughtRoutes(applications.Thoughts), Notes: NewReaderNoteRoutes(applications.Notes), Inbox: NewReaderInboxRoutes(applications.Inbox),
			Todos: NewReaderTodoRoutes(applications.Todos), Library: NewReaderLibraryRoutes(applications.Library), Hosts: NewReaderHostRoutes(applications.Hosts),
		}
	}
	var routes ReaderRoutes
	routes.Thoughts, _ = candidate.(ReaderThoughtRoutes)
	routes.Notes, _ = candidate.(ReaderNoteRoutes)
	routes.Inbox, _ = candidate.(ReaderInboxRoutes)
	routes.Todos, _ = candidate.(ReaderTodoRoutes)
	routes.Library, _ = candidate.(ReaderLibraryRoutes)
	routes.Hosts, _ = candidate.(ReaderHostRoutes)
	return routes
}

func readerServiceTestStores(store any) service.ReaderStores {
	var stores service.ReaderStores
	stores.Thoughts, _ = store.(service.ReaderThoughtStore)
	stores.Notes, _ = store.(service.ReaderNoteStore)
	stores.Inbox, _ = store.(service.ReaderInboxStore)
	stores.Todos, _ = store.(service.ReaderTodoStore)
	stores.Library, _ = store.(service.ReaderLibraryStore)
	stores.Hosts, _ = store.(service.ReaderHostStore)
	return stores
}
