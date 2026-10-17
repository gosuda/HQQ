# zstd_server: the generated glue (zstd_server.{h,c}), the hand-written hooks and main.c, and
# Zstandard built from source, linked against libhqq (BINDGEN.md 9.6).
#
# The zstd source is not in the repository. Fetch and verify the pinned release, then point
# HQQ_ZSTD_DIR at it (the directory that holds lib/zstd.h):
#
#   zdir=$(scripts/release/fetch-zstd.sh)
#   cmake -S bindgen/examples/zstd/server -B /tmp/hqq-zstd-build -G Ninja -DHQQ_ZSTD_DIR="$zdir"
#   cmake --build /tmp/hqq-zstd-build
#   # ASan + UBSan: add -DHQQ_SANITIZE=address+undefined (the knob of c/CMakeLists.txt)
#
# The binary is <build>/zstd_server and <build>/libhqqzstd.a is the zstd library built from the
# same source, which the cgo comparison package links (see README.md).
#
# This directory is the top-level project and pulls in libhqq from c/ with add_subdirectory.
# HQQ_WERROR defaults to ON for the glue and the hooks; zstd's own sources are third-party code and
# are built with their own flags. POSIX only: hqqrpc_main does not run on Windows (BINDGEN.md 1.2).
cmake_minimum_required(VERSION 3.21)

if(NOT TARGET hqq::hqq)
  project(zstd_server LANGUAGES C)
  if(WIN32)
    message(FATAL_ERROR "zstd_server is POSIX only: hqqrpc_main does not support Windows")
  endif()
  if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
  endif()
  set(HQQ_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../../../..")
  set(HQQ_BUILD_TESTS OFF)
  set(HQQ_BUILD_CXX_CHECK OFF)
  set(HQQ_BUILD_STRESS OFF)
  option(HQQ_WERROR "Treat compiler warnings as errors" ON)
  add_subdirectory("${HQQ_ROOT}/c" hqq)
  list(APPEND CMAKE_MODULE_PATH "${HQQ_ROOT}/c/cmake")
  include(HqqCompilerOptions)
endif()

set(HQQ_ZSTD_DIR "" CACHE PATH
    "Unpacked Zstandard source (the directory that holds lib/zstd.h); see scripts/release/fetch-zstd.sh")
if(NOT HQQ_ZSTD_DIR)
  message(FATAL_ERROR "set -DHQQ_ZSTD_DIR to the unpacked Zstandard source: "
                      "scripts/release/fetch-zstd.sh fetches and verifies the pinned release")
endif()
set(ZSTD_LIB "${HQQ_ZSTD_DIR}/lib")
foreach(f zstd.h zstd_errors.h common/zstd_common.c compress/zstd_compress.c decompress/zstd_decompress.c)
  if(NOT EXISTS "${ZSTD_LIB}/${f}")
    message(FATAL_ERROR "HQQ_ZSTD_DIR does not look like a Zstandard source tree: missing ${ZSTD_LIB}/${f}")
  endif()
endforeach()

# Compression and decompression only: no dictionary builder, no legacy formats, no worker threads
# (ZSTD_MULTITHREAD stays undefined, so ZSTD_c_nbWorkers is unsupported), no x86-64 assembly.
file(GLOB ZSTD_SOURCES CONFIGURE_DEPENDS
  "${ZSTD_LIB}/common/*.c" "${ZSTD_LIB}/compress/*.c" "${ZSTD_LIB}/decompress/*.c")
add_library(hqqzstd STATIC ${ZSTD_SOURCES})
target_include_directories(hqqzstd PUBLIC "${ZSTD_LIB}")
target_compile_definitions(hqqzstd PRIVATE ZSTD_DISABLE_ASM=1 ZSTD_LEGACY_SUPPORT=0)
set_target_properties(hqqzstd PROPERTIES C_VISIBILITY_PRESET default POSITION_INDEPENDENT_CODE ON)
if(HQQ_SANITIZER_FLAGS)
  target_compile_options(hqqzstd PRIVATE ${HQQ_SANITIZER_FLAGS})
endif()

add_executable(zstd_server
  main.c
  zstd_server.c
  zstd_hooks.c)
target_include_directories(zstd_server PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
target_link_libraries(zstd_server PRIVATE hqq::hqq hqqzstd)
hqq_apply_compile_options(zstd_server)
