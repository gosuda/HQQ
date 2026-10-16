# Unit tests. Every C test is a single file test_<name>.c; a test is registered
# when its file exists. Tests that need the library internals (hqq_internal.h,
# hqq_ring.h) link the static `hqq_internal` target; tests of the public API
# link `hqq::hqq`.
#
#   hqq_add_test(<name> [INTERNAL] [POSIX_ONLY] [TIMEOUT <s>] [LABELS <l>...] [SOURCES <extra.c>...])

add_library(hqq_testutil STATIC hqq_testutil.c)
target_link_libraries(hqq_testutil PUBLIC hqq_internal)
target_include_directories(hqq_testutil PUBLIC ${CMAKE_CURRENT_SOURCE_DIR})
hqq_apply_compile_options(hqq_testutil)

function(hqq_add_test name)
  cmake_parse_arguments(T "INTERNAL;POSIX_ONLY" "TIMEOUT" "LABELS;SOURCES" ${ARGN})
  if(T_POSIX_ONLY AND WIN32)
    return()
  endif()
  set(src ${CMAKE_CURRENT_SOURCE_DIR}/test_${name}.c)
  if(NOT EXISTS ${src})
    return()
  endif()
  add_executable(test_${name} ${src} ${T_SOURCES})
  if(T_INTERNAL)
    target_link_libraries(test_${name} PRIVATE hqq_testutil hqq_internal)
  else()
    target_link_libraries(test_${name} PRIVATE hqq_testutil hqq::hqq)
  endif()
  target_include_directories(test_${name} PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR} ${CMAKE_CURRENT_SOURCE_DIR}/generated)
  hqq_apply_compile_options(test_${name})
  add_test(NAME ${name} COMMAND test_${name})
  if(NOT T_TIMEOUT)
    set(T_TIMEOUT 120)
  endif()
  set_tests_properties(${name} PROPERTIES TIMEOUT ${T_TIMEOUT})
  if(T_LABELS)
    set_tests_properties(${name} PROPERTIES LABELS "${T_LABELS}")
  endif()
endfunction()

hqq_add_test(smoke)
hqq_add_test(constants INTERNAL)
hqq_add_test(ring INTERNAL)
hqq_add_test(superblock INTERNAL)
hqq_add_test(link INTERNAL)
hqq_add_test(adv)
hqq_add_test(listener INTERNAL)
hqq_add_test(simple)
hqq_add_test(named INTERNAL POSIX_ONLY)
hqq_add_test(fork POSIX_ONLY LABELS posix)
hqq_add_test(stress LABELS stress TIMEOUT 180)

if(HQQ_BUILD_CXX_CHECK)
  add_executable(test_cxx17 test_cxx17.cpp)
  target_link_libraries(test_cxx17 PRIVATE hqq::hqq)
  hqq_apply_compile_options(test_cxx17)
  add_test(NAME cxx17 COMMAND test_cxx17)
endif()

# The generated constants header must match spec/constants.json.
find_package(Python3 COMPONENTS Interpreter)
if(Python3_FOUND)
  add_test(NAME constants_fresh
           COMMAND ${Python3_EXECUTABLE} ${CMAKE_CURRENT_SOURCE_DIR}/../tools/gen_constants.py --check)
endif()
