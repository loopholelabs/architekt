sudo drafter-forwarder --port-forwards '[
  {
    "netns": "ark0",
    "internalPort": "6379",
    "protocol": "tcp",
    "externalAddr": "127.0.0.1:3333"
  }
]'
