#!/usr/bin/bash

GOARCH=arm go build ./cmd/nanotdb/
scp nanotdb rpi:nanotdb/nanotdb

