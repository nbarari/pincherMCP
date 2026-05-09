terraform {
  required_version = ">= 1.5.0"
}

provider "aws" {
  region = var.region
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = [var.ami_name_filter]
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

module "network" {
  source = "./modules/network"

  cidr_block = local.network_cidr
  region     = var.region
}

resource "aws_instance" "web" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = var.instance_type
  subnet_id     = module.network.public_subnet_id

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_instance" "worker" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = local.worker_size
  subnet_id     = module.network.private_subnet_id

  tags = local.common_tags
}

resource "aws_security_group" "web_sg" {
  name        = "${var.stack_name}-web"
  description = "Allow web traffic"
  vpc_id      = module.network.vpc_id

  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
