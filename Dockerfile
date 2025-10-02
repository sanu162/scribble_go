# Use official Go image
FROM golang:1.25-alpine

# Set working directory inside the container
WORKDIR /app

# Install air for live reloading (optional, very useful for dev)
RUN apk add --no-cache git \
    && go install github.com/cosmtrek/air@v1.43.0

# Copy go.mod and go.sum first to leverage caching
COPY go.mod go.sum ./
RUN go mod download

# Copy all source code
COPY . .

# Expose the port your app uses
EXPOSE 8080

# Run air for hot reload in development
CMD ["air"]
