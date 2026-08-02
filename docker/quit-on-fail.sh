#!/bin/sh
# supervisor 事件监听器：任一被管进程进入 FATAL 就杀掉 supervisord，
# 让容器整体退出、由 Docker 的 restart 策略重来一遍。
#
# 不这么做的话，容器会处于"进程挂了但容器还在跑"的僵尸态——
# 健康检查可能还过得去（另一个进程活着），排查起来非常费劲。
printf "READY\n"
while read -r _line; do
  printf "RESULT 2\nOK" 
  echo "[supervisor] 检测到子进程 FATAL，终止容器以触发重启策略" >&2
  kill -TERM 1
  printf "READY\n"
done
