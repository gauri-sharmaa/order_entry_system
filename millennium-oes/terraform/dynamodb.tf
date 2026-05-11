# -----------------------------------------------------------------------
# DynamoDB — durable order persistence
#
# Why DynamoDB:
#   - Single-digit millisecond reads at any scale
#   - Serverless — no capacity planning
#   - TTL for automatic cleanup of old orders
#   - On-demand mode — pay per request, no idle cost
#
# Table design (single-table):
#   PK: ORDER#<id>
#   SK: ORDER#<id>
#   GSI1: symbol + created_at (query orders by symbol)
#   GSI2: status + created_at (query by status)
# -----------------------------------------------------------------------

resource "aws_dynamodb_table" "orders" {
  name         = "${var.app_name}-orders"
  billing_mode = "PAY_PER_REQUEST"  # on-demand, no capacity planning
  hash_key     = "PK"
  range_key    = "SK"

  attribute {
    name = "PK"
    type = "S"
  }
  attribute {
    name = "SK"
    type = "S"
  }
  attribute {
    name = "symbol"
    type = "S"
  }
  attribute {
    name = "created_at"
    type = "S"
  }
  attribute {
    name = "status"
    type = "S"
  }

  # GSI: query orders by symbol, sorted by time
  global_secondary_index {
    name            = "symbol-created-index"
    hash_key        = "symbol"
    range_key       = "created_at"
    projection_type = "ALL"
  }

  # GSI: query orders by status
  global_secondary_index {
    name            = "status-created-index"
    hash_key        = "status"
    range_key       = "created_at"
    projection_type = "ALL"
  }

  # TTL — automatically delete orders older than 90 days
  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  # Point-in-time recovery
  point_in_time_recovery {
    enabled = true
  }

  # Encryption at rest
  server_side_encryption {
    enabled = true
  }

  tags = { Name = "${var.app_name}-orders" }
}
