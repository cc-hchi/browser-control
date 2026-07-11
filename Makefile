.PHONY: build check test acceptance skill-validate install uninstall install-skill uninstall-skill clean

build:
	go build ./...
	pnpm build

check:
	go vet ./...
	pnpm check

test:
	go test ./...
	pnpm test

acceptance:
	scripts/run-acceptance.sh

skill-validate:
	python3 "$${CODEX_HOME:-$$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" skills/control-local-chrome

install:
	scripts/install.sh

uninstall:
	scripts/uninstall.sh

install-skill:
	scripts/install-skill.sh

uninstall-skill:
	scripts/uninstall-skill.sh

clean:
	rm -rf bin dist coverage
	find . -type d -name node_modules -prune -exec rm -rf {} +
