# -----------------------------------------------------------------------
# ElastiCache Redis — hot order state store
#
# Why Redis for the hot path:
#   - Sub-millisecond reads/writes (in-memory)
#   - Atomic operations (no race conditions)
#   - Pub/Sub for future event broadcasting
#   - Sorted sets for order book if needed
#
# Node choice: r7g.large (Graviton3)
#   - r = memory-optimized (Redis is memory-bound)
#   - 7g = latest Graviton3 ARM — best price/perf on AWS
#   - large = 13.07 GB RAM, more than enough for millions of orders
# -----------------------------------------------------------------------

resource "aws_elasticache_subnet_group" "redis" {
  name       = "${var.app_name}-redis-subnet"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_elasticache_replication_group" "redis" {
  replication_group_id = "${var.app_name}-redis"
  description          = "Millennium OES order state cache"

  node_type            = var.redis_node_type
  num_cache_clusters   = 2  # 1 primary + 1 replica for HA
  port                 = 6379

  subnet_group_name    = aws_elasticache_subnet_group.redis.name
  security_group_ids   = [aws_security_group.redis.id]

  # Redis 7 — latest stable, includes Redis Functions
  engine_version       = "7.1"
  parameter_group_name = aws_elasticache_parameter_group.redis.name

  # Encryption
  at_rest_encryption_enabled  = true
  transit_encryption_enabled  = true

  # Automatic failover
  automatic_failover_enabled = true
  multi_az_enabled           = true

  # Maintenance
  maintenance_window       = "sun:05:00-sun:06:00"
  snapshot_retention_limit = 1
  snapshot_window          = "04:00-05:00"

  # Apply changes immediately (not during maintenance window)
  apply_immediately = true

  tags = { Name = "${var.app_name}-redis" }
}

resource "aws_elasticache_parameter_group" "redis" {
  name   = "${var.app_name}-redis-params"
  family = "redis7"

  # Tuned for low latency
  parameter {
    name  = "maxmemory-policy"
    value = "allkeys-lru"  # evict LRU keys when memory full
  }
  parameter {
    name  = "tcp-keepalive"
    value = "60"
  }
  parameter {
    name  = "timeout"
    value = "300"
  }
  # Disable slow log for production (reduces overhead)
  parameter {
    name  = "slowlog-log-slower-than"
    value = "10000"  # 10ms
  }
}
