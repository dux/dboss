#!/bin/sh
# Gives the demo its own repository on the first start; every later start finds .git and is done.
[ -e .git ] && exit 0
git init -q -b main &&
  git add -A &&
  git -c user.name=dboss -c user.email=dboss@vibe.lvh.me commit -q -m "Start the vibe demo" &&
  echo "vibe: initialized the demo repository"
