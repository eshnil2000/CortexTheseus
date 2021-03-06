/*!
 *  Copyright (c) 2017 by Contributors
 * \file module_util.h
 * \brief Helper utilities for module building
 */
#ifndef CVM_RUNTIME_CVMMODEL_H_
#define CVM_RUNTIME_CVMMODEL_H_

#include <cvm/dlpack.h>
#include <cvm/runtime/packed_func.h>

#include <string>
#include <mutex>

using std::string;

namespace cvm {
namespace runtime {

extern double transpose_int8_avx256_transpose_cnt;
extern double transpose_int8_avx256_gemm_cnt;
extern double im2col_cnt;
extern double cvm_op_cvm_shift_cnt;
extern double cvm_op_clip_cnt;
extern double cvm_op_dense_cnt;
extern double cvm_op_maxpool_cnt;
extern double cvm_op_broadcast_cnt;
extern double cvm_op_concat_cnt;
extern double cvm_op_upsampling_cnt;
extern double cvm_op_inline_matmul_cnt;
extern double cvm_op_elemwise_cnt;
extern double cvm_op_chnwise_conv_cnt;
extern double cvm_op_chnwise_conv1x1_cnt;
extern double cvm_op_depthwise_conv_cnt;

struct CVMModel {
public:
  CVMModel(const string& graph, DLContext _ctx);
  ~CVMModel();
  int LoadParams(const string& params_str);
  int LoadParamsFromFile(string filepath);
  int GetInputLength();
  int GetOutputLength();
  int64_t GetStorageSize();
  int64_t GetOps();
  int GetSizeOfOutputType();
  int GetSizeOfInputType();
  void Run(DLTensor* input, std::vector<DLTensor*> output);
  DLTensor* PlanInput(void*, int);
  std::vector<DLTensor*> PlanOutput();
  void SaveTensor(std::vector<DLTensor*> outputs, char *data);

  std::string GetVersion();
  std::string GetPostprocessMethod();
  bool SetPostprocessMethod(const string postprocess_method);
  bool IsReady() const;
private:
  void SetInput_(string index, DLTensor* input);
  void GetOutput_(int index, DLTensor* output);
  DLContext ctx_;
  PackedFunc set_input_;
  PackedFunc get_output_;
  PackedFunc load_params_;
  PackedFunc get_ops_;
  PackedFunc run_;
  PackedFunc get_storage_size_;
  Module module_;
  int64_t in_size_;
  int64_t *out_size_;
  int32_t out_num_;
  int8_t output_bytes_;
  int8_t input_bytes_;
  std::vector<int> dims_;
  std::vector<int64_t*> shapes_;
  std::string version_, postprocess_method_;
  int dtype_code{kDLInt};
  int dtype_bits{32};
  int dtype_lanes{1};
  int32_t input_num_{1};
  bool loaded_{false};
};

}
}

#endif // CVM_RUNTIME_CVMMODEL_H_
