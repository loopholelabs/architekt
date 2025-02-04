#!/bin/bash

sudo drafter-snapshotter --netns ark0 --devices '[
  {
    "name": "state",
    "output": "out/package/state.bin"
  },
  {
    "name": "memory",
    "output": "out/package/memory.bin"
  },
  {
    "name": "kernel",
    "input": "out/blueprint/vmlinux",
    "output": "out/package/vmlinux"
  },
  {
    "name": "disk",
    "input": "out/blueprint/rootfs.ext4",
    "output": "out/package/rootfs.ext4"
  },
  {
    "name": "config",
    "output": "out/package/config.json"
  },
  {
    "name": "oci",
    "input": "out/blueprint/oci.ext4",
    "output": "out/package/oci.ext4"
  }
]'
