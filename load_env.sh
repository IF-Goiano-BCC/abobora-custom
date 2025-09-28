#!/bin/bash

# Script to load environment variables from .env file
# Usage: source load_env.sh or ./load_env.sh

# Get the directory where this script is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

# Check if .env file exists
if [ ! -f "$ENV_FILE" ]; then
    echo "Error: .env file not found at $ENV_FILE"
    exit 1
fi

echo "Loading environment variables from $ENV_FILE"

# Read .env file and export variables
while IFS='=' read -r key value; do
    # Skip empty lines and comments
    if [[ -z "$key" || "$key" =~ ^[[:space:]]*# ]]; then
        continue
    fi
    
    # Remove leading/trailing whitespace
    key=$(echo "$key" | xargs)
    value=$(echo "$value" | xargs)
    
    # Remove quotes if present
    value=$(echo "$value" | sed 's/^["'"'"']//g' | sed 's/["'"'"']$//g')
    
    # Export the variable
    export "$key"="$value"
    echo "Set $key=$value"
done < "$ENV_FILE"

echo "Environment variables loaded successfully!"

# Optional: If you want to run this script directly (not sourced),
# you can uncomment the following lines to execute a command with the loaded environment
# if [ $# -gt 0 ]; then
#     echo "Executing: $*"
#     exec "$@"
# fi
