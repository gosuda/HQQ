# microbench_server: the C server of the cgo comparison benchmarks (spec/toolchain/BINDGEN.md 10.5).
#
# Standalone (the way the benchmarks are built; Release matters, the C side is part of what is timed):
#
#   cmake -S bindgen/bench/server -B build/bench -G Ninja -DCMAKE_BUILD_TYPE=Release
#   cmake --build build/bench
#
# This builds libhqq from c/ as a subdirectory, without its tests. The binary is
# build/bench/microbench_server. -DHQQ_WERROR=ON and -DHQQ_SANITIZE=address+undefined apply to the
# server as they do to libhqq. When c/CMakeLists.txt adds this directory instead, libhqq and its
# compile options are already there and the standalone preamble is skipped.
if(NOT TARGET hqq)
  cmake_minimum_required(VERSION 3.21)
  project(microbench_server LANGUAGES C)
  if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
    set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
  endif()
  set(HQQ_BUILD_TESTS OFF CACHE BOOL "" FORCE)
  set(HQQ_BUILD_CXX_CHECK OFF CACHE BOOL "" FORCE)
  set(HQQ_BUILD_STRESS OFF CACHE BOOL "" FORCE)
  add_subdirectory("${CMAKE_CURRENT_LIST_DIR}/../../../c" hqq)
  # The variables the options function reads (warning and sanitizer flags) live in the scope of
  # c/CMakeLists.txt; include the module here to have them in this one.
  list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_LIST_DIR}/../../../c/cmake")
  include(HqqCompilerOptions)
endif()

add_executable(microbench_server
  main.c
  microbench_server.c
  microbench_hooks.c
  ../../testdata/microlib/microlib.c)
target_include_directories(microbench_server PRIVATE ../../testdata/microlib)
target_link_libraries(microbench_server PRIVATE hqq)
hqq_apply_compile_options(microbench_server)
