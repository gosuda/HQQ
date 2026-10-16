# C++17 binding for libhqq (header-only wrapper: include/hqq/hqq.hpp).
#
#   cmake -S bindings/cpp -B build-cpp -DCMAKE_BUILD_TYPE=Release
#   cmake --build build-cpp
#   ctest --test-dir build-cpp --output-on-failure
#
# Targets
#   hqq::cpp              INTERFACE target: the wrapper header, links hqq::hqq
#   hqq_cpp_test          unit tests (tests/test_hqq.cpp)
#   hqq_cpp_echo          echo example over a Standard link
#   hqq_cpp_rpc           request/response example over an Advanced link
#   hqq-interop           interop helper (interop/hqq_interop.cpp, conformance/INTEROP_CLI.md)
#
# The C library is built from the in-tree sources (c/) unless the parent project
# already defines hqq::hqq. Options:
#   HQQ_CPP_BUILD_TESTS     ON    build tests, examples and the interop helper
#   HQQ_CPP_EXCEPTIONS      OFF   compile the tests with HQQ_CPP_EXCEPTIONS=1
#   HQQ_CPP_WERROR          OFF   warnings are errors for the binding targets
#   HQQ_CPP_SANITIZE        none  address+undefined, address, undefined or thread
#   HQQ_CPP_HQQ_ROOT        ../../c  the C library source tree
cmake_minimum_required(VERSION 3.21)

project(hqq_cpp VERSION 0.1.0 LANGUAGES C CXX)

if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
  set(HQQ_CPP_IS_TOP_LEVEL ON)
else()
  set(HQQ_CPP_IS_TOP_LEVEL OFF)
endif()

option(HQQ_CPP_BUILD_TESTS "Build the C++ tests, examples and interop helper" ${HQQ_CPP_IS_TOP_LEVEL})
option(HQQ_CPP_EXCEPTIONS "Compile the binding tests with HQQ_CPP_EXCEPTIONS=1" OFF)
option(HQQ_CPP_WERROR "Treat warnings as errors in the binding targets" OFF)
set(HQQ_CPP_SANITIZE "none" CACHE STRING "Sanitizer for the C library and the binding: none, address, undefined, address+undefined, thread")
set_property(CACHE HQQ_CPP_SANITIZE PROPERTY STRINGS none address undefined address+undefined thread)
set(HQQ_CPP_HQQ_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../../c" CACHE PATH "Source tree of libhqq (the c/ directory)")

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# ---- libhqq ----------------------------------------------------------------
if(NOT TARGET hqq::hqq)
  # The C project's own tests, tools and extras are not needed here.
  set(HQQ_BUILD_TESTS OFF)
  set(HQQ_BUILD_CXX_CHECK OFF)
  set(HQQ_BUILD_STRESS OFF)
  set(HQQ_BUILD_SHARED OFF)
  set(HQQ_SANITIZE "${HQQ_CPP_SANITIZE}")
  if(HQQ_CPP_WERROR)
    set(HQQ_WERROR ON)
  endif()
  add_subdirectory("${HQQ_CPP_HQQ_ROOT}" "${CMAKE_CURRENT_BINARY_DIR}/hqq-c")
endif()

# ---- sanitizers for the C++ side (the C side gets them through HQQ_SANITIZE) --
set(HQQ_CPP_SANITIZER_FLAGS "")
if(NOT HQQ_CPP_SANITIZE STREQUAL "none")
  if(MSVC)
    message(FATAL_ERROR "HQQ_CPP_SANITIZE is supported with GCC and Clang only")
  endif()
  if(HQQ_CPP_SANITIZE STREQUAL "address")
    set(_hqq_san address)
  elseif(HQQ_CPP_SANITIZE STREQUAL "undefined")
    set(_hqq_san undefined)
  elseif(HQQ_CPP_SANITIZE STREQUAL "address+undefined")
    set(_hqq_san address,undefined)
  elseif(HQQ_CPP_SANITIZE STREQUAL "thread")
    set(_hqq_san thread)
  else()
    message(FATAL_ERROR "Unknown HQQ_CPP_SANITIZE value '${HQQ_CPP_SANITIZE}'")
  endif()
  set(HQQ_CPP_SANITIZER_FLAGS -fsanitize=${_hqq_san} -fno-omit-frame-pointer -g)
  if(HQQ_CPP_SANITIZE MATCHES "undefined")
    list(APPEND HQQ_CPP_SANITIZER_FLAGS -fno-sanitize-recover=undefined)
  endif()
endif()

# ---- the header-only wrapper -----------------------------------------------
add_library(hqq_cpp INTERFACE)
add_library(hqq::cpp ALIAS hqq_cpp)
target_include_directories(hqq_cpp INTERFACE
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>)
target_link_libraries(hqq_cpp INTERFACE hqq::hqq)
target_compile_features(hqq_cpp INTERFACE cxx_std_17)

if(HQQ_CPP_BUILD_TESTS)
  find_package(Threads REQUIRED)
  enable_testing()

  function(hqq_cpp_target_setup target)
    target_link_libraries(${target} PRIVATE hqq::cpp Threads::Threads)
    if(MSVC)
      target_compile_options(${target} PRIVATE /W4 $<$<BOOL:${HQQ_CPP_WERROR}>:/WX>)
    else()
      target_compile_options(${target} PRIVATE -Wall -Wextra -Wpedantic
        $<$<BOOL:${HQQ_CPP_WERROR}>:-Werror>)
    endif()
    if(HQQ_CPP_SANITIZER_FLAGS)
      target_compile_options(${target} PRIVATE ${HQQ_CPP_SANITIZER_FLAGS})
      target_link_options(${target} PRIVATE ${HQQ_CPP_SANITIZER_FLAGS})
    endif()
    if(HQQ_CPP_EXCEPTIONS)
      target_compile_definitions(${target} PRIVATE HQQ_CPP_EXCEPTIONS=1)
    endif()
  endfunction()

  add_executable(hqq_cpp_test tests/test_hqq.cpp)
  hqq_cpp_target_setup(hqq_cpp_test)
  add_test(NAME hqq_cpp_test COMMAND hqq_cpp_test)

  add_executable(hqq_cpp_echo examples/echo.cpp)
  hqq_cpp_target_setup(hqq_cpp_echo)
  add_executable(hqq_cpp_rpc examples/rpc.cpp)
  hqq_cpp_target_setup(hqq_cpp_rpc)

  add_executable(hqq_cpp_interop interop/hqq_interop.cpp)
  set_target_properties(hqq_cpp_interop PROPERTIES OUTPUT_NAME hqq-interop)
  hqq_cpp_target_setup(hqq_cpp_interop)

  add_test(NAME hqq_cpp_echo_selftest
           COMMAND hqq_cpp_echo --selftest)
  add_test(NAME hqq_cpp_rpc_selftest
           COMMAND hqq_cpp_rpc --selftest)
endif()
