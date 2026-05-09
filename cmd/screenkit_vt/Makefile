# Build the macOS native capture+encode helper for webrtc-vnc.
#
#   make            -> ./screenkit_vt (release, universal arm64+x86_64)
#   make install    -> copies the binary to ../../screenkit_vt next to webrtc-vnc
#   make clean
#
# Requires: Xcode command-line tools (xcrun swiftc).

BIN      := screenkit_vt
SRC      := main.swift
ARCHS    := arm64 x86_64
SDK      := macosx
SWIFTC   := xcrun -sdk $(SDK) swiftc
COMMON   := -O -framework ScreenCaptureKit -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework IOKit -framework Foundation

ARCH_FLAGS := $(foreach a,$(ARCHS),-target $(a)-apple-macos12.3)

.PHONY: all clean install

all: $(BIN)

$(BIN): $(SRC)
	@for a in $(ARCHS); do \
		$(SWIFTC) $(COMMON) -target $$a-apple-macos12.3 -o $(BIN)-$$a $(SRC) || exit 1; \
	done
	lipo -create -output $(BIN) $(foreach a,$(ARCHS),$(BIN)-$(a))
	@rm -f $(foreach a,$(ARCHS),$(BIN)-$(a))
	@echo "built $(BIN) (arch: $(ARCHS))"

install: $(BIN)
	cp $(BIN) ../../$(BIN)
	@echo "installed -> ../../$(BIN)"

clean:
	rm -f $(BIN) $(BIN)-arm64 $(BIN)-x86_64
