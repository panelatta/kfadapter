// This file only fences the frontend tree off from the Go module above, so
// `go test ./...` never descends into npm packages that ship Go sources.
module kfadapter.invalid/web-placeholder

go 1.26.0
