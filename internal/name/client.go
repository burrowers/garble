package name

import "net/rpc"

type Client struct {
	rpcClient *rpc.Client
}

func (c *Client) GetPackageName(info *PackageInfo) string {
	var reply string
	if err := c.rpcClient.Call("Server.GetPackageName", info, &reply); err != nil {
		panic(err)
	}
	return reply
}

func (c *Client) GetFieldName(info *FieldInfo) string {
	var reply string
	if err := c.rpcClient.Call("Server.GetFieldName", info, &reply); err != nil {
		panic(err)
	}
	return reply
}

func SetupClient(addr string) (*Client, error) {
	client, err := rpc.Dial("tcp4", addr)
	if err != nil {
		return nil, err
	}
	return &Client{rpcClient: client}, nil
}
