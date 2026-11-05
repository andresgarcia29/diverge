#!/bin/sh
set -e

# Diverge Environment Engine Operator Installer

error() {
    echo "Error: $1" >&2
}

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

if [ "$ARCH" = "x86_64" ]; then
    ARCH="amd64"
elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    ARCH="arm64"
else
    error "Unsupported architecture: $ARCH"
    exit 1
fi

REPO="divergedev/diverge"

TAG=$(curl -sI "https://github.com/$REPO/releases/latest" | grep -i "^location:" | sed -n 's/.*\/tag\/\([^[:space:]]*\).*/\1/p' | tr -d '\r')

if [ "$TAG" = "latest" ] || [ -z "$TAG" ]; then
  error "Could not determine latest release tag"
  exit 1
fi

VERSION="${TAG#v}"
ARCHIVE="diverge_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$TAG/$ARCHIVE"
CHECKSUM_URL="https://github.com/$REPO/releases/download/$TAG/checksums.txt"

echo "Downloading release $TAG..."
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "$DOWNLOAD_URL" -o "$TMP_DIR/$ARCHIVE"
curl -fsSL "$CHECKSUM_URL" -o "$TMP_DIR/checksums.txt"

echo "Verifying checksum..."
(
    cd "$TMP_DIR"
    if command -v sha256sum >/dev/null 2>&1; then
        if ! grep " ${ARCHIVE}\$" checksums.txt | sha256sum -c -; then
            echo "Error: Checksum verification failed!" >&2
            exit 1
        fi
    elif command -v shasum >/dev/null 2>&1; then
        if ! grep " ${ARCHIVE}\$" checksums.txt | shasum -a 256 -c -; then
            echo "Error: Checksum verification failed!" >&2
            exit 1
        fi
    else
        echo "Error: Neither sha256sum nor shasum found for checksum verification!" >&2
        exit 1
    fi
)

echo "Extracting..."
tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"

if [ ! -f "$TMP_DIR/diverge" ]; then
    error "Binary 'diverge' not found in archive."
    exit 1
fi

INSTALL_DIR="/usr/local/bin"
if [ ! -w "$INSTALL_DIR" ]; then
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
fi

echo "Installing diverge to $INSTALL_DIR..."
mv "$TMP_DIR/diverge" "$INSTALL_DIR/diverge"
chmod +x "$INSTALL_DIR/diverge"

echo "Installation complete."

"$INSTALL_DIR/diverge" version

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "Please add $INSTALL_DIR to your PATH." ;;
esac
