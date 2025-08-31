group "default" {
    targets = ["build"]
}

group "test" {
    targets = ["linux-64", "linux-32-amd", "linux-32-arm"] 
}

target "build" {
    dockerfile = "garble.Dockerfile"
    target = "garble"
    tags = ["garble:latest"]
    // Will inherit the os and arch of the host machine
}

target "linux-64" {
    dockerfile = "garble.Dockerfile"
    target = "build"
    platforms = [ 
        "linux/amd64",
        "linux/arm64",
        "linux/riscv64",
        "linux/mips64le",
        "linux/ppc64le",
    ]
}

target "linux-32-amd" {
    dockerfile = "garble.Dockerfile"
    target = "build"
    platforms = [ 
        "linux/386",
    ]
}

target "linux-32-arm" {
    dockerfile = "garble.Dockerfile"
    target = "build-32-arm"
    platforms = [ 
        "linux/arm/v6",
    ]
}