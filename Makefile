DATABASE_URL ?= postgres://postgres:dev@127.0.0.1:55432/aigw

.PHONY: db ui build run dev-ui test bundle

db: ## local Postgres in podman
	podman run -d --name aigw-ui-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=aigw -p 127.0.0.1:55432:5432 docker.io/library/postgres:17

ui:
	cd web && npm install && npm run build

build: ui
	go build -o bin/aigw-ui ./cmd/server

run: ## needs ADMIN_TOKEN and ENCRYPTION_KEY in the environment
	DATABASE_URL=$(DATABASE_URL) go run ./cmd/server

dev-ui: ## Vite dev server, proxies /api to :8080
	cd web && npm run dev

test:
	go test ./...

bundle: ## image + chart for a disconnected install, needs podman and helm
	deploy/offline/build-bundle.sh
