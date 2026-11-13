/* Choose which embedded decrypt kernel image to load for a device.
 *
 * build-native-cuda.sh embeds a compute_80 PTX baseline and, when nvcc knows
 * the architecture, SASS cubins for sm_90 and sm_100. A cubin for sm_XY runs
 * on devices with compute capability X.Z for Z >= Y (same major), so sm_90
 * covers 9.x and sm_100 covers 10.x. Anything else (8.x, 12.x, unknown)
 * takes the PTX, which the driver JIT-compiles. Shared by gpu_cuda.c and
 * the host tests (kernel_select_host.c) so the rule is tested without a GPU. */
#ifndef RACER_KERNEL_SELECT_H
#define RACER_KERNEL_SELECT_H

#define RACER_KERNEL_IMAGE_PTX 0
#define RACER_KERNEL_IMAGE_SM90 1
#define RACER_KERNEL_IMAGE_SM100 2

/* Best image for compute capability major.minor given which cubins are
 * embedded. Returns RACER_KERNEL_IMAGE_PTX when no cubin matches. */
static inline int racer_kernel_select(int major, int minor, int have_sm90, int have_sm100) {
    if (minor < 0) return RACER_KERNEL_IMAGE_PTX;
    if (major == 9 && have_sm90) return RACER_KERNEL_IMAGE_SM90;
    if (major == 10 && have_sm100) return RACER_KERNEL_IMAGE_SM100;
    return RACER_KERNEL_IMAGE_PTX;
}

#endif
