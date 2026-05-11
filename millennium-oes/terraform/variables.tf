variable "aws_region" {
  description = "AWS region — us-east-1 is closest to Alpaca's NY infrastructure"
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Environment name (paper, prod)"
  type        = string
  default     = "paper"
}

variable "app_name" {
  description = "Application name"
  type        = string
  default     = "millennium-oes"
}

variable "alpaca_api_key" {
  description = "Alpaca API key"
  type        = string
  sensitive   = true
}

variable "alpaca_api_secret" {
  description = "Alpaca API secret"
  type        = string
  sensitive   = true
}

variable "alpaca_base_url" {
  description = "Alpaca base URL"
  type        = string
  default     = "https://paper-api.alpaca.markets"
}

variable "alpaca_stream_url" {
  description = "Alpaca WebSocket stream URL"
  type        = string
  default     = "wss://paper-api.alpaca.markets/stream"
}

variable "ecs_cpu" {
  description = "ECS task CPU units (1024 = 1 vCPU)"
  type        = number
  default     = 1024
}

variable "ecs_memory" {
  description = "ECS task memory in MB"
  type        = number
  default     = 2048
}

variable "ecs_desired_count" {
  description = "Number of ECS tasks to run"
  type        = number
  default     = 2
}

variable "redis_node_type" {
  description = "ElastiCache Redis node type"
  type        = string
  default     = "cache.r7g.large"  # r7g = Graviton3, low latency
}

variable "container_image" {
  description = "Docker image URI (ECR)"
  type        = string
  default     = ""  # set after pushing to ECR
}
