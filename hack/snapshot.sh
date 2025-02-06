sudo drafter-snapshotter --netns ark0 --devices '[
  {
    "name": "state",
    "output": "image/package/state.bin"
  },
  {
    "name": "memory",
    "output": "image/package/memory.bin"
  },
  {
    "name": "kernel",
    "input": "image/blueprint/vmlinux",
    "output": "image/package/vmlinux"
  },
  {
    "name": "disk",
    "input": "image/blueprint/rootfs.ext4",
    "output": "image/package/rootfs.ext4"
  },
  {
    "name": "config",
    "output": "image/package/config.json"
  },
  {
    "name": "oci",
    "input": "image/blueprint/oci.ext4",
    "output": "image/package/oci.ext4"
  }
]'
