# Compose paths are relative to the project root, including .env selections.
COMPOSE_DEFAULT_FILES := compose/docker-compose.yml
COMPOSE_DOTENV_SELECTION := $(shell sed -nE 's/^[[:space:]]*(export[[:space:]]+)?COMPOSE_FILE[[:space:]]*=[[:space:]]*([^\#]*).*$$/\2/p' .env 2>/dev/null | tr -d "\"'" | tail -n 1)
COMPOSE_SELECTED_FILES = $(subst :, ,$(or $(strip $(COMPOSE_FILE)),$(strip $(COMPOSE_DOTENV_SELECTION)),$(COMPOSE_DEFAULT_FILES)))
# Accept legacy bare filenames while private .env selections are migrated.
COMPOSE_FILES = $(foreach file,$(patsubst ./%,%,$(COMPOSE_SELECTED_FILES)),$(if $(findstring /,$(file)),$(file),$(if $(wildcard compose/$(file)),compose/$(file),$(file))))
COMPOSE_FLAGS = $(foreach file,$(COMPOSE_FILES),-f "$(file)")
