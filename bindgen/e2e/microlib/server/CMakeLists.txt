# microlib_server: the generated glue (microlib_server.{h,c}), the hand-written hooks and main.c, and
# the MIT test library bindgen/testdata/microlib, linked against libhqq (BINDGEN.md 9.6).
#
# Two ways to build it:
#
#   1. Standalone, the way the Go e2e tests do it (this directory is the top-level project and pulls
#      in libhqq from c/ with add_subdirectory; nothing under c/ changes):
#
#        cmake -S bindgen/e2e/microlib/server -B build-ml -G Ninja -DCMAKE_BUILD_TYPE=Release
#        cmake --build build-ml --target microlib_server
#        # ASan + UBSan: add -DHQQ_SANITIZE=address+undefined (the same knob as c/CMakeLists.txt)
#
#      The binary is build-ml/microlib_server. HQQ_WERROR defaults to ON here.
#
#   2. As a subdirectory of a parent CMake project that already defines hqq::hqq:
#      then the existing hqq::hqq target is reused.
#
cmake_minimum_required(VERSION 3.21)

if(NOT TARGET hqq::hqq)
  project(microlib_server LANGUAGES C)
  set(HQQ_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../../../..")
  # Only the library is needed: no tests, no C++ consumer check, no stress harness.
  set(HQQ_BUILD_TESTS OFF)
  set(HQQ_BUILD_CXX_CHECK OFF)
  set(HQQ_BUILD_STRESS OFF)
  option(HQQ_WERROR "Treat compiler warnings as errors" ON)
  add_subdirectory("${HQQ_ROOT}/c" hqq)
  # The flags and the sanitizer selection of libhqq live in variables of its directory scope: load
  # them here, so that the server's own sources are compiled with the same flags.
  list(APPEND CMAKE_MODULE_PATH "${HQQ_ROOT}/c/cmake")
  include(HqqCompilerOptions)
endif()

set(MICROLIB_DIR "${CMAKE_CURRENT_SOURCE_DIR}/../../../testdata/microlib")

add_executable(microlib_server
  main.c
  microlib_server.c
  microlib_hooks.c
  "${MICROLIB_DIR}/microlib.c")
target_include_directories(microlib_server PRIVATE "${MICROLIB_DIR}" "${CMAKE_CURRENT_SOURCE_DIR}")
target_link_libraries(microlib_server PRIVATE hqq::hqq)
hqq_apply_compile_options(microlib_server)
