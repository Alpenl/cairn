package lockkey

const (
	// ClassSubmit identifies the shared URL-mutation advisory namespace.
	// Its value is a cross-process protocol constant and must remain stable
	// across replicas and rolling upgrades.
	ClassSubmit int32 = 1
	// Classes 2 through 4 are retired namespaces. Their values stay reserved so
	// mixed-version processes cannot reinterpret an old lock as a new one.
	// ClassSchemaMigration identifies the migration-runner namespace. It
	// serializes whole migration runs against one database, so that a
	// CREATE INDEX CONCURRENTLY still building on one runner is never mistaken
	// for an abandoned invalid index by another runner's recovery probe. This
	// value is a cross-process protocol constant and must not be reused.
	ClassSchemaMigration int32 = 5
	// ObjectSchemaMigration is the singleton object within the migration
	// namespace, forming the stable advisory key (5, 0).
	ObjectSchemaMigration int32 = 0
)
