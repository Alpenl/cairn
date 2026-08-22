package migrate

import _ "embed"

//go:embed install_schema.sql
var currentInstallSchemaSQL string
