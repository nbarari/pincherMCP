output "web_instance_id" {
  description = "ID of the web instance"
  value       = aws_instance.web.id
}

output "vpc_id" {
  description = "VPC ID from the network module"
  value       = module.network.vpc_id
}

output "available_zones" {
  description = "Availability zones in the active region"
  value       = data.aws_availability_zones.available.names
}
