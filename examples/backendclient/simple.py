import grpc
from backend_pb2 import Profile
import backend_pb2_grpc as backend

if __name__ == "__main__":
    channel = grpc.insecure_channel('localhost:50505')
    stub = backend.APIStub(channel)

    profile = Profile(id="foo", properties="")
    result = stub.CreateMatch(profile)
    print result
