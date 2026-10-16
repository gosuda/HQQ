# HQQ C reference library (libhqq) - scaffold.
#
#   cmake -S c -B build -DHQQ_WERROR=ON -DHQQ_SANITIZE=address+undefined
#   cmake --build build
#   ctest --test-dir build --output-on-failure
cmake_minimum_required(VERSION 3.21)

project(hqq VERSION 0.0.0 LANGUAGES C)

list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/cmake")

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
  set(HQQ_IS_TOP_LEVEL ON)
else()
  set(HQQ_IS_TOP_LEVEL OFF)
endif()

option(HQQ_WERROR "Treat compiler warnings as errors" OFF)
option(HQQ_BUILD_TESTS "Build the unit tests" ${HQQ_IS_TOP_LEVEL})
option(HQQ_BUILD_CXX_CHECK "Build the C++17 consumer compile check" ON)
option(HQQ_BUILD_STRESS "Build the bounded weak-memory stress litmus harness (POSIX threads)" ON)
option(HQQ_BUILD_SHARED "Build libhqq as a shared library" OFF)
set(HQQ_SANITIZE "none" CACHE STRING
    "Sanitizer to enable: none, address, undefined, address+undefined, thread")
set_property(CACHE HQQ_SANITIZE PROPERTY STRINGS
             none address undefined address+undefined thread)

if(HQQ_BUILD_CXX_CHECK)
  enable_language(CXX)
endif()

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
set(CMAKE_C_VISIBILITY_PRESET hidden)

include(HqqCompilerOptions)

# Probe <stdatomic.h> the way a consumer would use it, instead of trusting
# __STDC_NO_ATOMICS__ (MSVC keeps defining it with /experimental:c11atomics).
include(CheckCSourceCompiles)
set(CMAKE_REQUIRED_FLAGS "${HQQ_C11_ATOMICS_FLAGS_STR}")
check_c_source_compiles("
#include <stdatomic.h>
#include <stdint.h>
#if ATOMIC_LLONG_LOCK_FREE != 2
#error not lock-free
#endif
int main(void) {
  _Atomic uint64_t x;
  atomic_init(&x, 1);
  atomic_thread_fence(memory_order_seq_cst);
  return (int)atomic_fetch_add_explicit(&x, 1, memory_order_seq_cst) - 1;
}" HQQ_HAVE_C11_ATOMICS)
unset(CMAKE_REQUIRED_FLAGS)
if(NOT HQQ_HAVE_C11_ATOMICS)
  message(FATAL_ERROR
    "The C compiler does not provide always-lock-free C11 <stdatomic.h> atomics. "
    "On Windows use clang-cl, or MSVC 17.5+ (see spec/toolchain/C_TOOLCHAIN.md).")
endif()

add_library(hqq ${HQQ_LIB_TYPE} src/hqq_smoke.c)
add_library(hqq::hqq ALIAS hqq)
target_include_directories(hqq PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>)
hqq_apply_compile_options(hqq)
set_target_properties(hqq PROPERTIES
  VERSION ${PROJECT_VERSION}
  C_VISIBILITY_PRESET hidden)
if(HQQ_BUILD_SHARED)
  target_compile_definitions(hqq PUBLIC HQQ_SHARED=1)
endif()
if(HQQ_C11_ATOMICS_FORCE_DEFINE)
  target_compile_definitions(hqq PUBLIC HQQ_FORCE_C11_ATOMICS=1)
endif()

if(HQQ_BUILD_TESTS)
  enable_testing()
  add_subdirectory(tests)
endif()

if(HQQ_BUILD_TESTS AND HQQ_BUILD_STRESS AND NOT WIN32)
  add_subdirectory(verify/stress)
endif()
