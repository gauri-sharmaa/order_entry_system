# -----------------------------------------------------------------------
# ECR — container registry
# -----------------------------------------------------------------------

resource "aws_ecr_repository" "app" {
  name                 = var.app_name
  image_tag_mutability = "MUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = { Name = var.app_name }
}

# -----------------------------------------------------------------------
# ECS Cluster — using Fargate for serverless containers
# Fargate removes the need to manage EC2 instances
# -----------------------------------------------------------------------

resource "aws_ecs_cluster" "main" {
  name = "${var.app_name}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = { Name = "${var.app_name}-cluster" }
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = 1
  }
}

# -----------------------------------------------------------------------
# IAM — ECS task execution role
# -----------------------------------------------------------------------

resource "aws_iam_role" "ecs_execution" {
  name = "${var.app_name}-ecs-execution-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Task role — what the running container can do
resource "aws_iam_role" "ecs_task" {
  name = "${var.app_name}-ecs-task-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy" "ecs_task_dynamo" {
  name = "dynamo-access"
  role = aws_iam_role.ecs_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "dynamodb:PutItem",
        "dynamodb:GetItem",
        "dynamodb:UpdateItem",
        "dynamodb:Query",
        "dynamodb:Scan"
      ]
      Resource = [
        aws_dynamodb_table.orders.arn,
        "${aws_dynamodb_table.orders.arn}/index/*"
      ]
    }]
  })
}

# Allow reading secrets from SSM Parameter Store
resource "aws_iam_role_policy" "ecs_task_ssm" {
  name = "ssm-access"
  role = aws_iam_role.ecs_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ssm:GetParameter", "ssm:GetParameters"]
      Resource = "arn:aws:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:parameter/${var.app_name}/*"
    }]
  })
}

# -----------------------------------------------------------------------
# Secrets — Alpaca credentials stored in SSM Parameter Store
# -----------------------------------------------------------------------

resource "aws_ssm_parameter" "alpaca_key" {
  name  = "/${var.app_name}/alpaca_api_key"
  type  = "SecureString"
  value = var.alpaca_api_key
}

resource "aws_ssm_parameter" "alpaca_secret" {
  name  = "/${var.app_name}/alpaca_api_secret"
  type  = "SecureString"
  value = var.alpaca_api_secret
}

# -----------------------------------------------------------------------
# ECS Task Definition
# -----------------------------------------------------------------------

resource "aws_ecs_task_definition" "app" {
  family                   = var.app_name
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = var.ecs_cpu
  memory                   = var.ecs_memory
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name  = var.app_name
    image = var.container_image != "" ? var.container_image : "${aws_ecr_repository.app.repository_url}:latest"

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "PORT",               value = "8080" },
      { name = "AWS_REGION",         value = var.aws_region },
      { name = "DYNAMO_TABLE",       value = aws_dynamodb_table.orders.name },
      { name = "REDIS_ADDR",         value = "${aws_elasticache_replication_group.redis.primary_endpoint_address}:6379" },
      { name = "ALPACA_BASE_URL",    value = var.alpaca_base_url },
      { name = "ALPACA_STREAM_URL",  value = var.alpaca_stream_url },
    ]

    secrets = [
      {
        name      = "ALPACA_API_KEY"
        valueFrom = aws_ssm_parameter.alpaca_key.arn
      },
      {
        name      = "ALPACA_API_SECRET"
        valueFrom = aws_ssm_parameter.alpaca_secret.arn
      }
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.app.name
        "awslogs-region"        = var.aws_region
        "awslogs-stream-prefix" = "ecs"
      }
    }

    # Health check
    healthCheck = {
      command     = ["CMD-SHELL", "wget -qO- http://localhost:8080/api/risk || exit 1"]
      interval    = 10
      timeout     = 5
      retries     = 3
      startPeriod = 15
    }
  }])
}

# -----------------------------------------------------------------------
# ECS Service
# -----------------------------------------------------------------------

resource "aws_ecs_service" "app" {
  name            = var.app_name
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.app.arn
  desired_count   = var.ecs_desired_count
  launch_type     = "FARGATE"

  # Enable ECS Exec for debugging
  enable_execute_command = true

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.ecs.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = var.app_name
    container_port   = 8080
  }

  # Rolling deployment — zero downtime
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  deployment_controller {
    type = "ECS"
  }

  depends_on = [aws_lb_listener.http]
}

# -----------------------------------------------------------------------
# Auto Scaling
# -----------------------------------------------------------------------

resource "aws_appautoscaling_target" "ecs" {
  max_capacity       = 10
  min_capacity       = 2
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.app.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

resource "aws_appautoscaling_policy" "cpu" {
  name               = "${var.app_name}-cpu-scaling"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.ecs.resource_id
  scalable_dimension = aws_appautoscaling_target.ecs.scalable_dimension
  service_namespace  = aws_appautoscaling_target.ecs.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = 60.0
    scale_in_cooldown  = 60
    scale_out_cooldown = 30
  }
}

# -----------------------------------------------------------------------
# CloudWatch Logs
# -----------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/${var.app_name}"
  retention_in_days = 30
}
