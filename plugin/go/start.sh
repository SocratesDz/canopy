#!/bin/sh
set -e

echo "[canoliq] starting canopy node..."
./canopy start &
NODE_PID=$!

echo "[canoliq] waiting for plugin socket..."
until [ -S /tmp/plugin/plugin.sock ]; do
  if ! kill -0 $NODE_PID 2>/dev/null; then
    echo "[canoliq] canopy node exited unexpectedly"
    exit 1
  fi
  sleep 1
done

echo "[canoliq] socket ready — starting canoLiq plugin"
exec ./plugin/go/go-plugin
