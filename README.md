# FaultTolerantKVService
一种具有强一致性的分布式键值对存储系统，它基于raft共识算法提供了一种可靠的方式来存储需要由分布式系统或机器集群访问的数据；支持Get、Put、Append、Delete四种。
它可以在网络分区期间进行领导者选举，并可以容忍机器故障，即使是领导者节点也是如此。
Server支持grpc调用，具备密码认证、会话管理、重启恢复等特性；Client支持在备机间自动故障转移。
