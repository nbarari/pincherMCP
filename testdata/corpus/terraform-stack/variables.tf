variable "region" {
  description = "AWS region to deploy into"
  type        = string
  default     = "us-east-1"
}

variable "stack_name" {
  description = "Prefix applied to named resources"
  type        = string
}

variable "instance_type" {
  description = "EC2 instance class for the web tier"
  type        = string
  default     = "t3.small"
}

variable "ami_name_filter" {
  description = "AMI name filter for the Ubuntu lookup"
  type        = string
  default     = "ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"
}

variable "owner_email" {
  description = "Tag value for resource ownership"
  type        = string
}
