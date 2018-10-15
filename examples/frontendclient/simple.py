import grpc
from frontend_pb2 import Group
import frontend_pb2_grpc as frontend
from random import randint

if __name__ == "__main__":
    channel = grpc.insecure_channel('localhost:50504')
    stub = frontend.APIStub(channel)

    player_id = randint(1, 100000)
    group = Group(id=str(player_id), properties="")
    result = stub.CreateRequest(group)
    print "Result: %s (Error: %s)" % (result.success,result.error,)
