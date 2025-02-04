#!/bin/bash

sudo drafter-runner --netns ark0 --devices '[
  {
    "name": "state",
    "path": "out/package/state.bin"
  },
  {
    "name": "memory",
    "path": "out/package/memory.bin"
  },
  {
    "name": "kernel",
    "path": "out/package/vmlinux"
  },
  {
    "name": "disk",
    "path": "out/package/rootfs.ext4"
  },
  {
    "name": "config",
    "path": "out/package/config.json"
  },
  {
    "name": "oci",
    "path": "out/blueprint/oci.ext4"
  }
]'
