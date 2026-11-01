#!/usr/bin/env bash
# 起一个真实的单节点 HDFS（WebHDFS 开启）——供 live-storage-gate.sh 调用。
#
# 为什么用真 Hadoop 而不是 mock：WebHDFS 的真实语义（CREATE/OPEN 的
# noredirect + Location 两步走、404 FileNotFoundException、403 权限语义）
# 与 mock 差着十万八千里 ⇒ 2026-11-02 真机实测正是靠它查出 List 的
# 「404 错误 vs 空列表」跨后端不一致。
#
# ⚠️ 必须在容器**内**跑（本脚本假定被 docker exec 调用）：
#    apache/hadoop 镜像的 namenode/datanode 都得在容器内起。
set -euo pipefail

CONF=/opt/hadoop/etc/hadoop

cat > "$CONF/core-site.xml" <<'XML'
<?xml version="1.0"?>
<configuration>
  <property><name>fs.defaultFS</name><value>hdfs://localhost:8020</value></property>
  <property><name>hadoop.http.staticuser.user</name><value>root</value></property>
</configuration>
XML

cat > "$CONF/hdfs-site.xml" <<'XML'
<?xml version="1.0"?>
<configuration>
  <property><name>dfs.replication</name><value>1</value></property>
  <property><name>dfs.namenode.name.dir</name><value>/tmp/hdfs/nn</value></property>
  <property><name>dfs.datanode.data.dir</name><value>/tmp/hdfs/dn</value></property>
  <!-- 关权限校验：门禁要验「驱动语义」，不是「HDFS 权限模型」 -->
  <property><name>dfs.permissions.enabled</name><value>false</value></property>
  <property><name>dfs.webhdfs.enabled</name><value>true</value></property>
  <property><name>dfs.namenode.http-bind-address</name><value>0.0.0.0:9870</value></property>
  <property><name>dfs.datanode.address</name><value>0.0.0.0:9866</value></property>
  <property><name>dfs.datanode.ipc-address</name><value>0.0.0.0:8020</value></property>
  <!-- datanode 对外宣告 hostname：驱动 CREATE/OPEN 两步走拿到的 Location
       要能被**门禁主机**访问 ⇒ 不能用容器内网名 -->
  <property><name>dfs.datanode.hostname</name><value>localhost</value></property>
  <property><name>dfs.datanode.http.address</name><value>0.0.0.0:9864</value></property>
</configuration>
XML

echo "localhost" > "$CONF/workers"

export HADOOP_HOME=/opt/hadoop
export HADOOP_CONF_DIR="$CONF"

hdfs namenode -format -force -nonInteractive >/tmp/format.log 2>&1 || {
  tail -30 /tmp/format.log; exit 1; }

hdfs --daemon start namenode
hdfs --daemon start datanode

for _ in $(seq 1 90); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:9870/webhdfs/v1/?op=LISTSTATUS" || true)"
  case "$code" in 200|403) echo "WEBHDFS_READY"; exit 0 ;; esac
  sleep 1
done

echo "WebHDFS 未就绪" >&2
exit 1
