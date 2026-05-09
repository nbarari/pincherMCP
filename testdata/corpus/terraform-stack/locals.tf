locals {
  network_cidr = "10.0.0.0/16"

  worker_size = var.instance_type == "t3.small" ? "t3.medium" : var.instance_type

  base_tags = {
    Stack   = var.stack_name
    Owner   = var.owner_email
    Managed = "terraform"
  }

  derived_tags = {
    Region = var.region
    AZ     = data.aws_availability_zones.available.names[0]
  }

  common_tags = merge(local.base_tags, local.derived_tags)
}
