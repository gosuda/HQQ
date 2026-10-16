# Bounded weak-memory stress litmus harness (POSIX threads).
find_package(Threads REQUIRED)
add_executable(hqq_stress_litmus stress_litmus.c)
target_link_libraries(hqq_stress_litmus PRIVATE hqq::hqq Threads::Threads)
hqq_apply_compile_options(hqq_stress_litmus)
# ctest runs a short, bounded pass; CI runs a longer one on arm64 explicitly.
add_test(NAME stress_litmus
         COMMAND hqq_stress_litmus --seconds 0.5 --iterations 100000)
set_tests_properties(stress_litmus PROPERTIES LABELS "stress" TIMEOUT 60)
