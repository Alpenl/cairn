package app

import "os"

func readOpenAPISpec() ([]byte, error) {
	return os.ReadFile("assets/openapi.json")
}
