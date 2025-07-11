// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore (interfaces: KVStore)

// Package mocks is a generated GoMock package.
package mocks

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
)

// MockKVStore is a mock of KVStore interface.
type MockKVStore struct {
	ctrl     *gomock.Controller
	recorder *MockKVStoreMockRecorder
}

// MockKVStoreMockRecorder is the mock recorder for MockKVStore.
type MockKVStoreMockRecorder struct {
	mock *MockKVStore
}

// NewMockKVStore creates a new mock instance.
func NewMockKVStore(ctrl *gomock.Controller) *MockKVStore {
	mock := &MockKVStore{ctrl: ctrl}
	mock.recorder = &MockKVStoreMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockKVStore) EXPECT() *MockKVStoreMockRecorder {
	return m.recorder
}

// Delete mocks base method.
func (m *MockKVStore) Delete(arg0 string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Delete", arg0)
	ret0, _ := ret[0].(error)
	return ret0
}

// Delete indicates an expected call of Delete.
func (mr *MockKVStoreMockRecorder) Delete(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Delete", reflect.TypeOf((*MockKVStore)(nil).Delete), arg0)
}

// Get mocks base method.
func (m *MockKVStore) Get(arg0 string) ([]byte, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Get", arg0)
	ret0, _ := ret[0].([]byte)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// Get indicates an expected call of Get.
func (mr *MockKVStoreMockRecorder) Get(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Get", reflect.TypeOf((*MockKVStore)(nil).Get), arg0)
}

// GetTemplateData mocks base method.
func (m *MockKVStore) GetTemplateData(arg0 string) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetTemplateData", arg0)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetTemplateData indicates an expected call of GetTemplateData.
func (mr *MockKVStoreMockRecorder) GetTemplateData(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetTemplateData", reflect.TypeOf((*MockKVStore)(nil).GetTemplateData), arg0)
}

// ListKeys mocks base method.
func (m *MockKVStore) ListKeys(arg0, arg1 int) ([]string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ListKeys", arg0, arg1)
	ret0, _ := ret[0].([]string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ListKeys indicates an expected call of ListKeys.
func (mr *MockKVStoreMockRecorder) ListKeys(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ListKeys", reflect.TypeOf((*MockKVStore)(nil).ListKeys), arg0, arg1)
}

// ListKeysWithPrefix mocks base method.
func (m *MockKVStore) ListKeysWithPrefix(arg0, arg1 int, arg2 string) ([]string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ListKeysWithPrefix", arg0, arg1, arg2)
	ret0, _ := ret[0].([]string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ListKeysWithPrefix indicates an expected call of ListKeysWithPrefix.
func (mr *MockKVStoreMockRecorder) ListKeysWithPrefix(arg0, arg1, arg2 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ListKeysWithPrefix", reflect.TypeOf((*MockKVStore)(nil).ListKeysWithPrefix), arg0, arg1, arg2)
}

// Set mocks base method.
func (m *MockKVStore) Set(arg0 string, arg1 []byte) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Set", arg0, arg1)
	ret0, _ := ret[0].(error)
	return ret0
}

// Set indicates an expected call of Set.
func (mr *MockKVStoreMockRecorder) Set(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Set", reflect.TypeOf((*MockKVStore)(nil).Set), arg0, arg1)
}
