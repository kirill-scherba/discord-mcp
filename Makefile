# discord-mcp — Discord MCP server (webhook tools + DAVE voice bot)
#
# Build requires cartridge-gg/discordgo (DAVE fork) with a locally built
# libdave. See cartridge-gg/discordgo README for libdave build instructions.

LIBDAVE_DIR ?= $(HOME)/go/src/github.com/cartridge-gg/discordgo/dave/libdave/cpp
LIBDAVE_BUILD := $(LIBDAVE_DIR)/build
VCPKG_LIBDIR := $(LIBDAVE_BUILD)/vcpkg_installed/x64-linux/lib
VCPKG_LIBS := $(shell find $(VCPKG_LIBDIR) -maxdepth 1 -name '*.a' | sort | tr '\n' ' ')

CGO_CFLAGS := -I$(LIBDAVE_DIR)/includes
CGO_LDFLAGS := -L$(VCPKG_LIBDIR) $(LIBDAVE_BUILD)/libdave.a \
	-Wl,--start-group $(VCPKG_LIBS) -Wl,--end-group \
	-lstdc++ -lm -ldl -lpthread

.PHONY: all build libdave clean restart

all: build

build:
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
	go build -o discord-mcp .

# Rebuild libdave from the cartridge-gg fork checkout.
libdave:
	$(MAKE) -C $(LIBDAVE_DIR) install

# Restart the MCP gateway so the new binary is picked up.
restart:
	systemctl --user restart mcp-gateway

clean:
	rm -f discord-mcp
