#!/bin/bash
# 飞牛NAS系统专用版
set -e

APP_NAME="goProxy"
BUILD_DIR="./build"
NDK_DIR="$(pwd)/android-ndk-r26b"

echo "========================================"
echo "构建 Android 版 $APP_NAME"
echo "========================================"

# 检查 Go 和模块
if ! command -v go >/dev/null 2>&1; then
    echo "错误: Go未安装"
    exit 1
fi

echo "✓ Go版本: $(go version)"
echo "✓ 模块: $(cat go.mod 2>/dev/null | head -1 || echo '无go.mod')"

# 初始化/更新模块
if [ ! -f "go.mod" ]; then
    echo "初始化go模块..."
    go mod init goProxy
fi

echo "整理依赖..."
go mod tidy 2>&1 | grep -v "warning" || true

# 下载NDK（如果需要）
if [ ! -d "$NDK_DIR" ]; then
    echo "下载 Android NDK r26b..."
    wget -q --show-progress -O ndk.zip \
        https://dl.google.com/android/repository/android-ndk-r26b-linux.zip
    unzip -q ndk.zip && rm ndk.zip
fi

# 清理构建目录
rm -rf "$BUILD_DIR" 2>/dev/null || true
mkdir -p "$BUILD_DIR"

# 设置工具链路径
TOOLCHAIN="$NDK_DIR/toolchains/llvm/prebuilt/linux-x86_64/bin"

echo "----------------------------------------"
echo "方法1: 尝试纯Go静态编译（无CGO）"
echo "----------------------------------------"

# 尝试使用 netgo 标签进行静态编译
STATIC_FLAGS="-tags netgo,osusergo -ldflags='-s -w -extldflags=-static'"

# 1. Android ARM
echo "构建 Android ARM (静态)..."
if GOOS=android GOARCH=arm GOARM=7 CGO_ENABLED=0 \
    go build -tags netgo,osusergo -ldflags="-s -w -extldflags=-static" \
    -o "$BUILD_DIR/goProxy-arm-static" . 2>/dev/null; then
    echo "✓ ARM静态编译成功"
    mv "$BUILD_DIR/goProxy-arm-static" "$BUILD_DIR/goProxy-arm"
    file "$BUILD_DIR/goProxy-arm"
else
    echo "✗ ARM静态编译失败，尝试启用CGO"
fi

# 2. Android ARM64
echo "构建 Android ARM64 (静态)..."
if GOOS=android GOARCH=arm64 CGO_ENABLED=0 \
    go build -tags netgo,osusergo -ldflags="-s -w -extldflags=-static" \
    -o "$BUILD_DIR/goProxy-arm64-static" . 2>/dev/null; then
    echo "✓ ARM64静态编译成功"
    mv "$BUILD_DIR/goProxy-arm64-static" "$BUILD_DIR/goProxy-arm64"
    file "$BUILD_DIR/goProxy-arm64"
else
    echo "✗ ARM64静态编译失败，尝试启用CGO"
fi

echo "----------------------------------------"
echo "方法2: 启用CGO使用NDK工具链"
echo "----------------------------------------"

# 检查编译器
find_compiler() {
    local arch=$1
    local api=21  # Android 5.0+ 支持大部分设备
    
    case $arch in
        arm)
            local compilers=(
                "$TOOLCHAIN/armv7a-linux-androideabi${api}-clang"
                "$TOOLCHAIN/armv7a-linux-androideabi28-clang"
                "$TOOLCHAIN/arm-linux-androideabi${api}-clang"
            )
            ;;
        arm64)
            local compilers=(
                "$TOOLCHAIN/aarch64-linux-android${api}-clang"
                "$TOOLCHAIN/aarch64-linux-android28-clang"
            )
            ;;
        *)
            return 1
            ;;
    esac
    
    for compiler in "${compilers[@]}"; do
        if [ -f "$compiler" ]; then
            echo "$compiler"
            return 0
        fi
    done
    
    return 1
}

# 3. Android ARM with CGO
echo "构建 Android ARM (CGO)..."
ARM_CC=$(find_compiler arm)
if [ -n "$ARM_CC" ]; then
    echo "使用编译器: $(basename $ARM_CC)"
    
    export GOOS=android
    export GOARCH=arm
    export GOARM=7
    export CGO_ENABLED=1
    export CC="$ARM_CC"
    export CXX="${ARM_CC%clang}clang++"
    
    if go build -o "$BUILD_DIR/goProxy-arm-cgo" -ldflags="-s -w" -trimpath . 2>&1; then
        echo "✓ ARM CGO编译成功"
        mv "$BUILD_DIR/goProxy-arm-cgo" "$BUILD_DIR/goProxy-arm"
        file "$BUILD_DIR/goProxy-arm"
    else
        echo "✗ ARM CGO编译失败"
    fi
    
    unset GOOS GOARCH GOARM CGO_ENABLED CC CXX
fi

# 4. Android ARM64 with CGO
echo "构建 Android ARM64 (CGO)..."
ARM64_CC=$(find_compiler arm64)
if [ -n "$ARM64_CC" ]; then
    echo "使用编译器: $(basename $ARM64_CC)"
    
    export GOOS=android
    export GOARCH=arm64
    export CGO_ENABLED=1
    export CC="$ARM64_CC"
    export CXX="${ARM64_CC%clang}clang++"
    
    if go build -o "$BUILD_DIR/goProxy-arm64-cgo" -ldflags="-s -w" -trimpath . 2>&1; then
        echo "✓ ARM64 CGO编译成功"
        mv "$BUILD_DIR/goProxy-arm64-cgo" "$BUILD_DIR/goProxy-arm64"
        file "$BUILD_DIR/goProxy-arm64"
    else
        echo "✗ ARM64 CGO编译失败"
    fi
    
    unset GOOS GOARCH CGO_ENABLED CC CXX
fi

echo "----------------------------------------"
echo "方法3: 构建Linux测试版本"
echo "----------------------------------------"

# 5. Linux测试版本
echo "构建 Linux 测试版本..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -o "$BUILD_DIR/goProxy-linux" -ldflags="-s -w" -trimpath . && {
    echo "✓ Linux测试版本构建成功"
    file "$BUILD_DIR/goProxy-linux"
}

# 压缩所有文件
echo "----------------------------------------"
echo "压缩文件..."
if command -v upx >/dev/null 2>&1; then
    for file in "$BUILD_DIR"/*; do
        if [ -f "$file" ]; then
            upx --best "$file" 2>/dev/null && echo "✓ $(basename $file) 压缩成功" || true
        fi
    done
else
    echo "⚠ UPX未安装，跳过压缩"
fi

echo "========================================"
echo "构建完成!"
echo "========================================"

if [ -d "$BUILD_DIR" ]; then
    echo "生成的文件:"
    ls -lh "$BUILD_DIR"/
    
    echo ""
    echo "文件信息:"
    for f in "$BUILD_DIR"/*; do
        [ -f "$f" ] && echo "  $(basename "$f"): $(file "$f" | cut -d: -f2-)"
    done
fi

echo ""
echo "部署说明:"
echo "1. 复制到Android设备:"
echo "   adb push $BUILD_DIR/goProxy-arm /data/local/tmp/"
echo "2. 设置权限:"
echo "   adb shell chmod +x /data/local/tmp/goProxy-arm"
echo "3. 运行:"
echo "   adb shell /data/local/tmp/goProxy-arm"
echo "4. 访问:"
echo "   http://设备IP:5575/proxy?url=视频地址&thread=4&chunkSize=1024"